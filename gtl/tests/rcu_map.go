// Code generated by "../generate.py --prefix=rcuTest --PREFIX=RCUTest -DKEY=string -DVALUE=uint64 -DHASH=testhash --package=tests --output=rcu_map.go ../rcu_map.go.tpl". DO NOT EDIT.

package tests

import (
	"sync/atomic"
	"unsafe"
)

// RCUTestMap is a concurrent map. A reader can access the map without lock,
// regardless of background updates.  The writer side must coordinate using an
// external mutex if there are multiple writers. This map is linearizable.
//
// Example:
//
//	m := NewRCUTestMap(10)
//	go func() {  // writer
//	  m.Store("foo", "bar")
//	}()
//	go func() {  // reader
//	  val, ok := m.Load("foo")
//	}
type RCUTestMap struct {
	p unsafe.Pointer // *rcuTestMapState
}

// RCUTestMapState represents a fixed-size chained hash table. It can store up to
// maxCapacity key/value pairs.  Beyond that, the caller must create a new
// RCUTestMapState with a larger capacity.
type rcuTestMapState struct {
	log2Len     uint             // ==log2(len(table))
	mask        uint64           // == ^(log2Len-1)
	table       []unsafe.Pointer // *rcuTestMapNode
	n           int              // # of objects currently stored in the table
	maxCapacity int              // max # of object that can be stored
}

// RCUTestMapNode represents a hash bucket.
type rcuTestMapNode struct {
	key   string
	value uint64

	// next points to the next element in the same hash bucket
	next unsafe.Pointer // *rcuTestMapNode
}

func newRCUTestMapState(log2Len uint) *rcuTestMapState {
	len := int(1 << log2Len)
	table := &rcuTestMapState{
		log2Len:     log2Len,
		mask:        uint64(log2Len - 1),
		table:       make([]unsafe.Pointer, 1<<log2Len),
		maxCapacity: int(float64(len) * 0.8),
	}
	if table.maxCapacity < len {
		table.maxCapacity = len
	}
	return table
}

// NewRCUTestMap creates a new map. Arg initialLenHint suggests the the initial
// capacity.  If you plan to store 100 keys, then pass 100 as the value. If you
// don't know the capacity, pass 0 as initialLenHint.
func NewRCUTestMap(initialLenHint int) *RCUTestMap {
	log2Len := uint(3) // 8 nodes
	for (1 << log2Len) < initialLenHint {
		if log2Len > 31 {
			// TODO(saito) We could make the table to grow larger than 32 bits, but
			// doing so will break 32bit builds.
			panic(initialLenHint)
		}
		log2Len++
	}
	m := RCUTestMap{p: unsafe.Pointer(newRCUTestMapState(log2Len))}
	return &m
}

// Load finds a value with the given key. Returns false if not found.
func (m *RCUTestMap) Load(key string) (uint64, bool) {
	hash := testhash(key)
	table := (*rcuTestMapState)(atomic.LoadPointer(&m.p))
	b := int(hash & table.mask)
	node := (*rcuTestMapNode)(atomic.LoadPointer(&table.table[b]))
	for node != nil {
		if node.key == key {
			return node.value, true
		}
		node = (*rcuTestMapNode)(atomic.LoadPointer(&node.next))
	}
	var dummy uint64
	return dummy, false
}

// store returns false iff the table needs resizing.
func (t *rcuTestMapState) store(key string, value uint64) bool {
	var (
		hash     = testhash(key)
		b        = int(hash & t.mask)
		node     = (*rcuTestMapNode)(t.table[b])
		probeLen = 0
		prevNode *rcuTestMapNode
	)
	for node != nil {
		if node.key == key {
			newNode := *node
			newNode.value = value
			if prevNode == nil {
				atomic.StorePointer(&t.table[b], unsafe.Pointer(&newNode))
			} else {
				atomic.StorePointer(&prevNode.next, unsafe.Pointer(&newNode))
			}
			return true
		}
		prevNode = node
		node = (*rcuTestMapNode)(node.next)
		probeLen++
		if probeLen >= 4 && t.n >= t.maxCapacity {
			return false
		}
	}
	newNode := rcuTestMapNode{key: key, value: value}
	if prevNode == nil {
		atomic.StorePointer(&t.table[b], unsafe.Pointer(&newNode))
	} else {
		atomic.StorePointer(&prevNode.next, unsafe.Pointer(&newNode))
	}
	t.n++
	return true
}

// Store stores the value for the given key. If the key is already in the map,
// it updates the mapping to the given value.
//
// Caution: if Store() is going to be called concurrently, it must be serialized
// externally.
func (m *RCUTestMap) Store(key string, value uint64) {
	table := (*rcuTestMapState)(atomic.LoadPointer(&m.p))
	if table.store(key, value) {
		return
	}
	log2Len := table.log2Len + 1
	if log2Len > 31 {
		panic(log2Len)
	}
	newTable := newRCUTestMapState(log2Len)
	// Copy the contents of the old table over to the new table.
	for _, p := range table.table {
		node := (*rcuTestMapNode)(p)
		for node != nil {
			if !newTable.store(node.key, node.value) {
				panic(node)
			}
			node = (*rcuTestMapNode)(node.next)
		}
	}
	if !newTable.store(key, value) {
		panic(key)
	}
	atomic.StorePointer(&m.p, unsafe.Pointer(newTable))
}
