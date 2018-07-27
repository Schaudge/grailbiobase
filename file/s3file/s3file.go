// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache-2.0
// license that can be found in the LICENSE file.

// Package s3file implements grail file interface for S3.
package s3file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/grailbio/base/errorreporter"
	"github.com/grailbio/base/file"
	"github.com/grailbio/base/log"
	"github.com/pkg/errors"
)

// Path separator used by s3file.
const pathSeparator = "/"

type maxRetrier interface {
	MaxRetries() int
}

// Options defines options that can be given when creating an s3Impl
type Options struct {
	// ServerSideEncryption allows you to set the `ServerSideEncryption` value to use when
	// uploading files (e.g.  "AES256")
	ServerSideEncryption string
}

type s3Impl struct {
	provider ClientProvider
	options  Options
}

// s3Info implements file.Info interface.
type s3Info struct {
	name    string
	size    int64
	modTime time.Time
	etag    string // = GetObjectOutput.ETag
}

type s3Obj struct {
	obj *s3.Object
	cp  *string
}

type accessMode int

const (
	readonly  accessMode = iota // file is opened by Open.
	writeonly                   // file is opened by Create.

	// TODO(saito) Stop using s3 upload manager. Implement cross-file throttling
	// instead.
	uploadPartSize    = 16 << 20
	uploadParallelism = 16
)

// Operations on a file are internally implemented by a goroutine running
// handleRequests. Requests to handleRequests are sent through s3File.reqCh. The
// response to a request is sent through request.ch.
//
// The user-facing s3File methods, such as Read and Seek are implemented in the following way:
//
// - Create a chan response.
//
// - Send a request object through s3File.ch. The response channel is included
// in the request.  handleRequests() receives the request, handles the request,
// and sends the response.
//
// - Wait for a message from either the response channel or context.Done(),
// whichever comes first.

type requestType int

const (
	seekRequest requestType = iota
	readRequest
	statRequest
	writeRequest
	closeRequest
	abortRequest
)

type request struct {
	ctx     context.Context // context passed to Read, Seek, Close, etc.
	reqType requestType

	// For Read and Write
	buf []byte

	// For Seek
	off    int64
	whence int

	// For sending the response
	ch chan response
}

type response struct {
	n        int     // # of bytes read. Set only by Read.
	off      int64   // Seek location. Set only by Seek.
	info     *s3Info // Set only by Stat.
	err      error   // Any error
	uploader *s3Uploader
}

// s3File implements file.File interface.
type s3File struct {
	name     string         // "s3://bucket/key/.."
	provider ClientProvider // Used to create s3 clients.
	mode     accessMode

	bucket string // bucket part of "name".
	key    string // key part "name".

	reqCh chan request

	// The following fields are accessed only by the handleRequests thread.
	info *s3Info // File metadata. Filled on demand.

	// Active GetObject body reader. Created by a Read() request. Closed on Seek
	// or Close call.
	bodyReader io.ReadCloser

	// Seek offset.
	// INVARIANT: position >= 0 && (position > 0 ⇒ info != nil)
	position int64

	// Used by files opened for writing.
	uploader *s3Uploader
}

type s3Lister struct {
	ctx                         context.Context
	clients                     []s3iface.S3API
	dir, scheme, bucket, prefix string

	object  s3Obj
	objects []s3Obj
	token   *string
	err     error
	done    bool
	recurse bool
}

// s3Reader implements io.ReadSeeker for S3.
type s3Reader struct {
	ctx context.Context
	f   *s3File
}

// s3Reader implements a placeholder io.Writer for S3.
type s3Writer struct {
	ctx context.Context
	f   *s3File
}

func shouldRetry(path string, err error, clients *[]s3iface.S3API) bool {
	if err == nil {
		return false
	}
	if len(*clients) <= 1 {
		// No more alternate clients to try
		return false
	}
	if aerr, ok := err.(awserr.Error); ok {
		switch aerr.Code() {
		case s3.ErrCodeNoSuchBucket, s3.ErrCodeNoSuchKey:
			// No point in trying again.
			return false
		case "NotFound":
			// GetObject seems to return this error rather ErrCodeNoSuchKey
			return false
		default:
			// Should retry with a different ticket.
		}
	}
	*clients = (*clients)[1:]
	return true
}

// NewImplementation creates a new file.Implementation for S3. The provider is
// called to create s3 client objects.
func NewImplementation(provider ClientProvider, opts Options) file.Implementation {
	return &s3Impl{provider, opts}
}

// Run handler in a separate goroutine, then wait for either the handler to
// finish, or ctx to be cancelled.
func runRequest(ctx context.Context, handler func() response) response {
	ch := make(chan response)
	go func() {
		ch <- handler()
		close(ch)
	}()
	select {
	case res := <-ch:
		return res
	case <-ctx.Done():
		return response{err: fmt.Errorf("Request cancelled")}
	}
}

func (impl *s3Impl) internalOpen(ctx context.Context, path string, mode accessMode) (file.File, error) {
	_, bucket, key, err := ParseURL(path)
	if err != nil {
		return nil, err
	}
	var uploader *s3Uploader
	if mode == writeonly {
		resp := runRequest(ctx, func() response {
			u, err := newUploader(ctx, impl.provider, impl.options, path, bucket, key)
			return response{uploader: u, err: err}
		})
		if resp.err != nil {
			return nil, resp.err
		}
		uploader = resp.uploader
	}
	f := &s3File{
		name:     path,
		mode:     mode,
		provider: impl.provider,
		bucket:   bucket,
		key:      key,
		uploader: uploader,
		reqCh:    make(chan request, 16),
	}
	go f.handleRequests()
	return f, nil
}

// Open opens a file for reading. The provided path should be of form
// "bucket/key..."
func (impl *s3Impl) Open(ctx context.Context, path string) (file.File, error) {
	return impl.internalOpen(ctx, path, readonly)
}

// Create opens a file for writing.
func (impl *s3Impl) Create(ctx context.Context, path string) (file.File, error) {
	return impl.internalOpen(ctx, path, writeonly)
}

// String implements a human-readable description.
func (impl *s3Impl) String() string { return "s3" }

// List implements file.Implementation interface.
func (impl *s3Impl) List(ctx context.Context, dir string, recurse bool) file.Lister {
	scheme, bucket, key, err := ParseURL(dir)
	if err != nil {
		return &s3Lister{ctx: ctx, dir: dir, err: err}
	}
	clients, err := impl.provider.Get(ctx, "ListBucket", dir)
	if err != nil {
		return &s3Lister{ctx: ctx, dir: dir, err: err}
	}
	return &s3Lister{
		ctx:     ctx,
		clients: clients,
		dir:     dir,
		scheme:  scheme,
		bucket:  bucket,
		prefix:  key,
		recurse: recurse,
	}
}

// Stat implements file.Implementation interface.
func (impl *s3Impl) Stat(ctx context.Context, path string) (file.Info, error) {
	resp := runRequest(ctx, func() response {
		_, bucket, key, err := ParseURL(path)
		if err != nil {
			return response{err: err}
		}
		clients, err := impl.provider.Get(ctx, "GetObject", path)
		if err != nil {
			return response{err: err}
		}
		for {
			resp, err := clients[0].HeadObjectWithContext(ctx, &s3.HeadObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
			})
			if shouldRetry(path, err, &clients) {
				continue
			}
			if err != nil {
				return response{err: err}
			}
			if *resp.ETag == "" {
				return response{err: fmt.Errorf("stat %v: file does not exist", path)}
			}
			return response{info: &s3Info{
				name:    filepath.Base(path),
				size:    *resp.ContentLength,
				modTime: *resp.LastModified,
				etag:    *resp.ETag,
			}}
		}
	})
	return resp.info, resp.err
}

// Remove implements file.Implementation interface.
func (impl *s3Impl) Remove(ctx context.Context, path string) error {
	resp := runRequest(ctx, func() response {
		_, bucket, key, err := ParseURL(path)
		if err != nil {
			return response{err: err}
		}
		clients, err := impl.provider.Get(ctx, "DeleteObject", path)
		if err != nil {
			return response{err: err}
		}
		for {
			_, err = clients[0].DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			if shouldRetry(path, err, &clients) {
				continue
			}
			return response{err: err}
		}
	})
	return resp.err
}

func maxRetries(clients []s3iface.S3API) int {
	for _, client := range clients {
		if s, ok := client.(maxRetrier); ok && s.MaxRetries() > 0 {
			return s.MaxRetries()
		}
	}
	return defaultMaxRetries
}

func (f *s3File) handleRequests() {
	for req := range f.reqCh {
		switch req.reqType {
		case seekRequest:
			f.handleSeek(req)
		case readRequest:
			f.handleRead(req)
		case statRequest:
			f.handleStat(req)
		case writeRequest:
			f.handleWrite(req)
		case closeRequest:
			f.handleClose(req)
		case abortRequest:
			f.handleAbort(req)
		default:
			panic(fmt.Sprintf("Illegal request: %+v", req))
		}
		close(req.ch)
	}
}

// Name returns the name of the file.
func (f *s3File) Name() string {
	return f.name
}

func (f *s3File) Close(ctx context.Context) error {
	err := f.runRequest(ctx, request{reqType: closeRequest}).err
	close(f.reqCh)
	f.provider = nil
	return err
}

func (f *s3File) Discard(ctx context.Context) error {
	if f.mode != writeonly {
		return fmt.Errorf("discard %v: file is not opened in write mode", f.name)
	}
	err := f.runRequest(ctx, request{reqType: abortRequest}).err
	close(f.reqCh)
	f.provider = nil
	return err
}

func (f *s3File) String() string {
	return "s3://" + f.name
}

// Send a request to the handleRequests goroutine and wait for a response. The
// caller must set all the necessary fields in req, except ctx and ch, which are
// filled by this method. On ctx timeout or cancellation, returns a response
// with non-nil err field.
func (f *s3File) runRequest(ctx context.Context, req request) response {
	resCh := make(chan response, 1)
	req.ctx = ctx
	req.ch = resCh
	f.reqCh <- req
	select {
	case res := <-resCh:
		return res
	case <-ctx.Done():
		return response{err: fmt.Errorf("Request cancelled")}
	}
}

func (f *s3File) Stat(ctx context.Context) (file.Info, error) {
	res := f.runRequest(ctx, request{reqType: statRequest})
	if res.err != nil {
		return nil, res.err
	}
	return res.info, nil
}

func (f *s3File) handleStat(req request) {
	if err := f.maybeFillInfo(req.ctx); err != nil {
		req.ch <- response{err: err}
		return
	}
	if f.info == nil {
		panic(fmt.Sprintf("failed to fill stats in %+v", f))
	}
	req.ch <- response{info: f.info}
}

func newInfo(path string, output *s3.GetObjectOutput) *s3Info {
	return &s3Info{
		name:    filepath.Base(path),
		size:    *output.ContentLength,
		modTime: *output.LastModified,
		etag:    *output.ETag,
	}
}

func (f *s3File) maybeFillInfo(ctx context.Context) error {
	if f.info != nil {
		return nil
	}
	clients, err := f.provider.Get(ctx, "GetObject", f.name)
	if err != nil {
		return err
	}
	for {
		output, err := clients[0].GetObjectWithContext(ctx, &s3.GetObjectInput{
			Bucket: aws.String(f.bucket),
			Key:    aws.String(f.key)})
		if shouldRetry(f.name, err, &clients) {
			continue
		}
		if err != nil {
			return err
		}
		if output.Body == nil {
			panic("GetObject with nil Body")
		}
		output.Body.Close() // nolint: errcheck
		if *output.ETag == "" {
			return fmt.Errorf("read %v: File does not exist", f.name)
		}
		f.info = newInfo(f.name, output)
		return nil
	}
}

func (f *s3File) Reader(ctx context.Context) io.ReadSeeker {
	if f.mode != readonly {
		return file.NewErrorReader(fmt.Errorf("reader %v: file is not opened in read mode", f.name))
	}
	return &s3Reader{ctx: ctx, f: f}
}

func (f *s3File) Writer(ctx context.Context) io.Writer {
	if f.mode != writeonly {
		return file.NewErrorWriter(fmt.Errorf("writer %v: file is not opened in write mode", f.name))
	}
	return &s3Writer{ctx: ctx, f: f}
}

// Seek implements io.Seeker
func (r *s3Reader) Seek(offset int64, whence int) (int64, error) {
	res := r.f.runRequest(r.ctx, request{
		reqType: seekRequest,
		off:     offset,
		whence:  whence,
	})
	return res.off, res.err
}

// Seek implements io.Seeker
func (f *s3File) handleSeek(req request) {
	if err := f.maybeFillInfo(req.ctx); err != nil {
		req.ch <- response{off: f.position, err: err}
		return
	}
	var newPosition int64
	switch req.whence {
	case io.SeekStart:
		newPosition = req.off
	case io.SeekCurrent:
		newPosition = f.position + req.off
	case io.SeekEnd:
		newPosition = f.info.size + req.off
	default:
		req.ch <- response{off: f.position, err: fmt.Errorf("illegal whence: %d", req.whence)}
		return
	}
	if newPosition < 0 {
		req.ch <- response{off: f.position, err: fmt.Errorf("out-of-bounds seek")}
		return
	}
	if newPosition == f.position {
		req.ch <- response{off: f.position}
	}
	f.position = newPosition
	if f.bodyReader != nil {
		f.bodyReader.Close() // nolint: errcheck
		f.bodyReader = nil
	}
	req.ch <- response{off: f.position}
}

// Read implements io.Reader
func (r *s3Reader) Read(p []byte) (n int, err error) {
	res := r.f.runRequest(r.ctx, request{
		reqType: readRequest,
		buf:     p,
	})
	return res.n, res.err
}

func (f *s3File) startGetObjectRequest(ctx context.Context, client s3iface.S3API) error {
	if f.bodyReader != nil {
		panic("get request still active")
	}
	input := &s3.GetObjectInput{
		Bucket: aws.String(f.bucket),
		Key:    aws.String(f.key),
	}
	if f.position > 0 {
		// We either seeked or read before. So f.info must have been set.
		if f.info == nil {
			panic(fmt.Sprintf("read %v: nil info: %+v", f.name, f))
		}
		if f.position >= f.info.size {
			return io.EOF
		}
		input.Range = aws.String(fmt.Sprintf("bytes=%d-", f.position))
	}
	output, err := client.GetObjectWithContext(ctx, input)
	if err != nil {
		return err
	}
	if *output.ETag == "" {
		output.Body.Close() // nolint: errcheck
		return fmt.Errorf("read %v: File does not exist", f.name)
	}
	if f.info != nil && f.info.etag != *output.ETag {
		output.Body.Close() // nolint: errcheck
		return fmt.Errorf("read %v: File version changed from %v to %v", f.name, f.info.etag, *output.ETag)
	}
	f.bodyReader = output.Body // take ownership
	if f.info == nil {
		f.info = newInfo(f.name, output)
	}
	return nil
}

// Read implements io.Reader
func (f *s3File) handleRead(req request) {
	buf := req.buf
	clients, err := f.provider.Get(req.ctx, "GetObject", f.name)
	if err != nil {
		req.ch <- response{err: err}
		return
	}
	maxRetries := maxRetries(clients)
	retries := 0
	for len(buf) > 0 {
		if f.bodyReader == nil {
			err = f.startGetObjectRequest(req.ctx, clients[0])
			if shouldRetry(f.name, err, &clients) {
				continue
			}
			if err != nil {
				break
			}
		}
		var n int
		n, err = f.bodyReader.Read(buf)
		if n > 0 {
			buf = buf[n:]
			f.position += int64(n)
		}
		if err != nil {
			f.bodyReader.Close() // nolint: errcheck
			f.bodyReader = nil
			if err != io.EOF {
				err = errors.WithStack(err)
				retries++
				if retries <= maxRetries {
					log.Error.Printf("s3read %v: retrying (%d) GetObject after error %v",
						f.name, retries, err)
					continue
				}
			}
			break
		}
	}
	totalBytesRead := len(req.buf) - len(buf)
	req.ch <- response{n: totalBytesRead, err: err}
}

func (f *s3File) handleWrite(req request) {
	f.uploader.write(req.buf)
	req.ch <- response{n: len(req.buf), err: nil}
}

func (o s3Obj) name() string {
	if o.obj == nil {
		return *o.cp
	}
	return *o.obj.Key
}

type uploadChunk struct {
	client   s3iface.S3API
	uploadID string
	partNum  int64
	buf      []byte
}

// A helper class for driving s3manager.Uploader through an io.Writer-like
// interface. Its write() method will feed data incrementally to the uploader,
// and finish() will wait for all the uploads to finish.
type s3Uploader struct {
	ctx         context.Context
	client      s3iface.S3API
	bucket, key string
	uploadID    string

	// curBuf is only accessed by the handleRequests thread.
	curBuf      []byte
	nextPartNum int64

	bufPool sync.Pool
	reqCh   chan uploadChunk
	err     errorreporter.T
	sg      sync.WaitGroup
	mu      sync.Mutex
	parts   []*s3.CompletedPart
}

func newUploader(ctx context.Context, provider ClientProvider, opts Options, path, bucket, key string) (*s3Uploader, error) {
	clients, err := provider.Get(ctx, "PutObject", path)
	if err != nil {
		return nil, err
	}
	params := &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	// Add any non-default options
	if opts.ServerSideEncryption != "" {
		params.SetServerSideEncryption(opts.ServerSideEncryption)
	}

	u := &s3Uploader{
		ctx:         ctx,
		bucket:      bucket,
		key:         key,
		bufPool:     sync.Pool{New: func() interface{} { return make([]byte, uploadPartSize) }},
		nextPartNum: 1,
	}
	for {
		resp, err := clients[0].CreateMultipartUploadWithContext(ctx, params)
		if shouldRetry(path, err, &clients) {
			continue
		}
		if err != nil {
			return nil, err
		}
		u.client = clients[0]
		u.uploadID = *resp.UploadId
		if u.uploadID == "" {
			panic(fmt.Sprintf("empty uploadID: %+v", resp))
		}
		break
	}

	u.reqCh = make(chan uploadChunk, uploadParallelism)
	for i := 0; i < uploadParallelism; i++ {
		u.sg.Add(1)
		go u.uploadThread()
	}
	return u, nil
}

func (u *s3Uploader) uploadThread() {
	defer u.sg.Done()
	for chunk := range u.reqCh {
		params := &s3.UploadPartInput{
			Bucket:     aws.String(u.bucket),
			Key:        aws.String(u.key),
			Body:       bytes.NewReader(chunk.buf),
			UploadId:   aws.String(chunk.uploadID),
			PartNumber: &chunk.partNum,
		}
		resp, err := chunk.client.UploadPartWithContext(u.ctx, params)
		u.bufPool.Put(chunk.buf)
		if err != nil {
			u.err.Set(err)
			continue
		}
		partNum := chunk.partNum
		completed := &s3.CompletedPart{ETag: resp.ETag, PartNumber: &partNum}
		u.mu.Lock()
		u.parts = append(u.parts, completed)
		u.mu.Unlock()
	}
}

// write appends data to file. It can be called only by the request thread.
func (u *s3Uploader) write(buf []byte) {
	if len(buf) == 0 {
		panic("empty buf in write")
	}
	for len(buf) > 0 {
		if len(u.curBuf) == 0 {
			u.curBuf = u.bufPool.Get().([]byte)
			u.curBuf = u.curBuf[:0]
		}
		if cap(u.curBuf) != uploadPartSize {
			panic("empty buf")
		}
		space := u.curBuf[len(u.curBuf):cap(u.curBuf)]
		n := len(buf)
		if n < len(space) {
			copy(space, buf)
			u.curBuf = u.curBuf[0 : len(u.curBuf)+n]
			return
		}
		copy(space, buf)
		buf = buf[len(space):]
		u.curBuf = u.curBuf[0:cap(u.curBuf)]
		u.reqCh <- uploadChunk{client: u.client, uploadID: u.uploadID, partNum: u.nextPartNum, buf: u.curBuf}
		u.nextPartNum++
		u.curBuf = nil
	}
}

func (u *s3Uploader) abort() error {
	_, err := u.client.AbortMultipartUploadWithContext(u.ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(u.bucket),
		Key:      aws.String(u.key),
		UploadId: aws.String(u.uploadID),
	})
	return err
}

// finish finishes writing. It can be called only by the request thread.
func (u *s3Uploader) finish() error {
	if len(u.curBuf) > 0 {
		u.reqCh <- uploadChunk{client: u.client, uploadID: u.uploadID, partNum: u.nextPartNum, buf: u.curBuf}
		u.curBuf = nil
	}
	close(u.reqCh)
	u.sg.Wait()
	if u.err.Err() == nil {
		if len(u.parts) == 0 {
			// Special case: an empty file. CompleteMUltiPartUpload with empty parts causes an error,
			// so work around the bug by issuing a separate PutObject request.
			u.abort() // nolint: errcheck
			_, err := u.client.PutObjectWithContext(u.ctx, &s3.PutObjectInput{
				Bucket: aws.String(u.bucket),
				Key:    aws.String(u.key),
				Body:   bytes.NewReader(nil),
			})
			u.err.Set(err)
		} else {
			// Parts must be sorted in PartNumber order.
			sort.Slice(u.parts, func(i, j int) bool {
				return *u.parts[i].PartNumber < *u.parts[j].PartNumber
			})
			params := &s3.CompleteMultipartUploadInput{
				Bucket:          aws.String(u.bucket),
				Key:             aws.String(u.key),
				UploadId:        aws.String(u.uploadID),
				MultipartUpload: &s3.CompletedMultipartUpload{Parts: u.parts},
			}
			_, err := u.client.CompleteMultipartUploadWithContext(u.ctx, params)
			u.err.Set(err)
		}
	}
	if u.err.Err() != nil {
		u.abort() // nolint: errcheck
	}
	return u.err.Err()
}

func (w *s3Writer) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	res := w.f.runRequest(w.ctx, request{
		reqType: writeRequest,
		buf:     p,
	})
	return res.n, res.err
}

func (f *s3File) handleClose(req request) {
	var err error
	if f.uploader != nil {
		err = f.uploader.finish()
	} else if f.bodyReader != nil {
		if e := f.bodyReader.Close(); e != nil && err == nil {
			err = e
		}
	}
	req.ch <- response{err: err}
}

func (f *s3File) handleAbort(req request) {
	err := f.uploader.abort()
	req.ch <- response{err: err}
}

// Scan implements Lister.Scan
func (l *s3Lister) Scan() bool {
	for {
		if l.err != nil {
			return false
		}
		l.err = l.ctx.Err()
		if l.err != nil {
			return false
		}
		if len(l.objects) > 0 {
			l.object, l.objects = l.objects[0], l.objects[1:]
			ll := len(l.prefix)
			// Ignore keys whose path component isn't exactly equal to l.prefix.  For
			// example, if l.prefix="foo/bar", then we yield "foo/bar" and
			// "foo/bar/baz", but not "foo/barbaz".
			keyVal := l.object.name()
			if ll > 0 && len(keyVal) > ll {
				if l.prefix[ll-1] == '/' {
					// Treat prefix "foo/bar/" as "foo/bar".
					ll--
				}
				if keyVal[ll] != '/' {
					continue
				}
			}
			return true
		}
		if l.done {
			return false
		}

		var prefix string
		if l.showDirs() && !strings.HasSuffix(l.prefix, pathSeparator) && l.prefix != "" {
			prefix = l.prefix + pathSeparator
		} else {
			prefix = l.prefix
		}

		req := &s3.ListObjectsV2Input{
			Bucket:            aws.String(l.bucket),
			ContinuationToken: l.token,
			Prefix:            aws.String(prefix),
		}

		if l.showDirs() {
			req.Delimiter = aws.String(pathSeparator)
		}

		res, err := l.clients[0].ListObjectsV2WithContext(l.ctx, req)
		if shouldRetry(l.dir, err, &l.clients) {
			continue
		}
		if err != nil {
			l.err = err
			return false
		}
		l.token = res.NextContinuationToken
		l.objects = make([]s3Obj, 0, len(res.Contents)+len(res.CommonPrefixes))
		for _, objVal := range res.Contents {
			l.objects = append(l.objects, s3Obj{obj: objVal})
		}
		if l.showDirs() { // add the pseudo Dirs
			for _, cpVal := range res.CommonPrefixes {
				// Follow the Linux convention that directories do not come back with a trailing /
				// when read by ListDir.  To determine it is a directory, it is necessary to
				// call implementation.Stat on the path and check IsDir()
				pseudoDirName := *cpVal.Prefix
				if strings.HasSuffix(pseudoDirName, pathSeparator) {
					pseudoDirName = pseudoDirName[:len(pseudoDirName)-1]
				}
				l.objects = append(l.objects, s3Obj{cp: &pseudoDirName})
			}
		}

		l.done = len(l.objects) == 0 || !aws.BoolValue(res.IsTruncated)
	}
}

// Path implements Lister.Path
func (l *s3Lister) Path() string {
	return fmt.Sprintf("%s://%s/%s", l.scheme, l.bucket, l.object.name())
}

// Info implements Lister.Info
func (l *s3Lister) Info() file.Info {
	if obj := l.object.obj; obj != nil {

		return &s3Info{
			size:    *obj.Size,
			modTime: *obj.LastModified,
			etag:    *obj.ETag,
		}
	}
	return nil
}

// IsDir implements Lister.IsDir
func (l *s3Lister) IsDir() bool {
	return l.object.cp != nil
}

// Err returns an error, if any.
func (l *s3Lister) Err() error {
	return l.err
}

// Object returns the last object that was scanned.
func (l *s3Lister) Object() s3Obj {
	return l.object
}

// showDirs controls whether CommonPrefixes are returned during a scan
func (l *s3Lister) showDirs() bool {
	return !l.recurse
}

func (i *s3Info) Name() string       { return i.name }
func (i *s3Info) Size() int64        { return i.size }
func (i *s3Info) ModTime() time.Time { return i.modTime }

// ParseURL parses a path of form "s3://grail-bucket/dir/file" and returns
// ("s3", "grail-bucket", "dir/file", nil).
func ParseURL(url string) (scheme, bucket, key string, err error) {
	var suffix string
	scheme, suffix, err = file.ParsePath(url)
	if err != nil {
		return "", "", "", err
	}
	parts := strings.SplitN(suffix, pathSeparator, 2)
	if len(parts) == 1 {
		return scheme, parts[0], "", nil
	}
	return scheme, parts[0], parts[1], nil
}