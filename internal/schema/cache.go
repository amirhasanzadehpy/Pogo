package schema

import "sync"

type Cache struct {
	mu         sync.RWMutex
	graph      *Graph
	generation uint64
}

func (cache *Cache) Load() (*Graph, uint64) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.graph, cache.generation
}

func (cache *Cache) Replace(graph *Graph) uint64 {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if graph == nil {
		return cache.generation
	}
	cache.generation++
	cache.graph = graph
	return cache.generation
}
