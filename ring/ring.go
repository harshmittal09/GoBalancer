package ring

import (
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"sync"
)

const (
	VirtualNodeCount = 100
)

type Ring struct {
	hashArray []uint32
	hashMap   map[uint32]string
	mu        sync.RWMutex
}

func New() *Ring {
	return &Ring{
		hashArray: make([]uint32, 0),
		hashMap:   make(map[uint32]string),
	}
}

func (r *Ring) AddNode(add string) {

	r.mu.Lock()
	defer r.mu.Unlock()

	for i := 1; i <= VirtualNodeCount; i++ {
		key := fmt.Sprintf("%s#%d", add, i)

		hashed_val := hasher(key)

		if _, exists := r.hashMap[hashed_val]; exists {
			continue
		}

		idx := r.getIndex(hashed_val)

		r.hashArray = append(r.hashArray, 0)
		copy(r.hashArray[idx+1:], r.hashArray[idx:])
		r.hashArray[idx] = hashed_val

		r.hashMap[hashed_val] = add
	}
}

func (r *Ring) GetNode(add string) string {
	hashed_val := hasher(add)

	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.hashArray) == 0 {
		return ""
	}

	idx := r.getIndex(hashed_val)

	var backendIdx uint32

	if idx == len(r.hashArray) {
		backendIdx = r.hashArray[0]
	} else {
		backendIdx = r.hashArray[idx]
	}

	return r.hashMap[backendIdx]

}

func hasher(key string) uint32 {
	hashFunc := fnv.New32a()
	hashFunc.Write([]byte(key))

	return hashFunc.Sum32()
}

func (r *Ring) getIndex(val uint32) int {
	idx := sort.Search(len(r.hashArray), func(i int) bool {
		return r.hashArray[i] >= val
	})

	return idx
}

func (r *Ring) RemoveNode(add string) {

	r.mu.Lock()
	defer r.mu.Unlock()

	for hashed_val, v := range r.hashMap {
		if v == add {
			idx := r.getIndex(hashed_val)

			r.hashArray = slices.Delete(r.hashArray, idx, idx+1)

			delete(r.hashMap, hashed_val)
		}
	}
}

func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.hashArray)
}
