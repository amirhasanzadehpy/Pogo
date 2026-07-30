package schema

import (
	"sync"
	"sync/atomic"
)

type cacheState struct {
	graph      *Graph
	generation uint64
}

type Cache struct {
	mu    sync.Mutex
	state atomic.Pointer[cacheState]
}

func (cache *Cache) Load() (*Graph, uint64) {
	state := cache.state.Load()
	if state == nil {
		return nil, 0
	}
	return state.graph, state.generation
}

func (cache *Cache) Replace(graph *Graph) uint64 {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	current := cache.state.Load()
	generation := uint64(0)
	if current != nil {
		generation = current.generation
	}
	if graph == nil {
		return generation
	}
	generation++
	cache.state.Store(&cacheState{graph: graph, generation: generation})
	return generation
}
