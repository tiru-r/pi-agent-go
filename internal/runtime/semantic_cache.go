package runtime

import (
	"container/list"
	"sync"
)

const (
	semanticCacheDefaultThreshold = 0.85
	semanticCacheDefaultCap       = 512
)

type cacheEntry struct {
	prompt   string
	response string
	trigrams map[string]struct{}
}

// SemanticCache caches (prompt → response) pairs using Jaccard similarity
// over character 3-grams as a semantic proxy. LRU eviction at cap entries.
// Safe for concurrent use.
type SemanticCache struct {
	mu        sync.Mutex
	threshold float64
	cap       int
	list      *list.List            // MRU front, LRU back
	items     map[string]*list.Element // prompt → list element
}

// NewSemanticCache creates a cache with the given similarity threshold and capacity.
func NewSemanticCache(threshold float64, cap int) *SemanticCache {
	if cap <= 0 {
		cap = semanticCacheDefaultCap
	}
	if threshold <= 0 {
		threshold = semanticCacheDefaultThreshold
	}
	return &SemanticCache{
		threshold: threshold,
		cap:       cap,
		list:      list.New(),
		items:     make(map[string]*list.Element, cap),
	}
}

// trigrams returns the set of character 3-grams in s.
func trigrams(s string) map[string]struct{} {
	if len(s) < 3 {
		out := make(map[string]struct{}, 1)
		out[s] = struct{}{}
		return out
	}
	out := make(map[string]struct{}, len(s))
	for i := 0; i <= len(s)-3; i++ {
		out[s[i:i+3]] = struct{}{}
	}
	return out
}

// jaccard returns the Jaccard similarity of two trigram sets.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	intersection := 0
	for k := range a {
		if _, ok := b[k]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 1
	}
	return float64(intersection) / float64(union)
}

// Lookup returns (response, true) if a cached entry has Jaccard similarity
// ≥ threshold with prompt; otherwise returns ("", false).
func (sc *SemanticCache) Lookup(prompt string) (string, bool) {
	tg := trigrams(prompt)
	sc.mu.Lock()
	defer sc.mu.Unlock()

	// Exact match first (fast path).
	if el, ok := sc.items[prompt]; ok {
		sc.list.MoveToFront(el)
		return el.Value.(*cacheEntry).response, true
	}

	// Similarity scan — O(n) but n ≤ 512.
	var best *list.Element
	bestSim := 0.0
	for el := sc.list.Front(); el != nil; el = el.Next() {
		e := el.Value.(*cacheEntry)
		sim := jaccard(tg, e.trigrams)
		if sim > bestSim {
			bestSim = sim
			best = el
		}
	}
	if bestSim >= sc.threshold && best != nil {
		sc.list.MoveToFront(best)
		return best.Value.(*cacheEntry).response, true
	}
	return "", false
}

// Store inserts or updates a (prompt, response) pair, evicting LRU if at cap.
func (sc *SemanticCache) Store(prompt, response string) {
	tg := trigrams(prompt)
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if el, ok := sc.items[prompt]; ok {
		sc.list.MoveToFront(el)
		e := el.Value.(*cacheEntry)
		e.response = response
		e.trigrams = tg
		return
	}

	if sc.list.Len() >= sc.cap {
		back := sc.list.Back()
		if back != nil {
			sc.list.Remove(back)
			delete(sc.items, back.Value.(*cacheEntry).prompt)
		}
	}

	entry := &cacheEntry{prompt: prompt, response: response, trigrams: tg}
	el := sc.list.PushFront(entry)
	sc.items[prompt] = el
}
