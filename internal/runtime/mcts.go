package runtime

import (
	"math"
	"math/rand"
	"sort"
)

// mctsNode represents one state in the MCTS tree.
// State = (remaining budget, set of probes already selected).
type mctsNode struct {
	parent   *mctsNode
	children []*mctsNode

	// action that led to this node (probe index into the probes slice)
	actionIdx int

	visits  float64
	value   float64 // cumulative reward
	budget  float64
	runMask uint64 // bitmask of probes already run (max 64 probes supported)
}

func (n *mctsNode) ucb1(totalVisits float64, c float64) float64 {
	if n.visits == 0 {
		return math.Inf(1)
	}
	return n.value/n.visits + c*math.Sqrt(math.Log(totalVisits)/n.visits)
}

// MCTSPlan selects the best set of stale probes to run within budget using MCTS.
// Returns at most len(stale) probes in priority order. Falls back gracefully
// when iterations ≤ 0 or probes is empty.
func MCTSPlan(probes []ProbeSpec, budget float64, iterations int) []ProbeSpec {
	if len(probes) == 0 || iterations <= 0 {
		return nil
	}
	// Cap at 64 to fit runMask.
	if len(probes) > 64 {
		probes = probes[:64]
	}

	root := &mctsNode{actionIdx: -1, budget: budget}
	const ucbC = 1.414

	// Local RNG avoids global lock contention and makes simulations independent.
	localRng := rand.New(rand.NewSource(rand.Int63())) //nolint:gosec

	for range iterations {
		// Selection: traverse to a promising leaf.
		node := root
		for len(node.children) > 0 {
			var best *mctsNode
			bestUCB := math.Inf(-1)
			for _, ch := range node.children {
				u := ch.ucb1(node.visits+1, ucbC)
				if u > bestUCB {
					bestUCB = u
					best = ch
				}
			}
			node = best
		}

		// Expansion: add children for each un-run probe that fits.
		if node.visits > 0 || node == root {
			for i, p := range probes {
				if node.runMask&(1<<uint(i)) != 0 {
					continue
				}
				if p.Overhead > node.budget {
					continue
				}
				child := &mctsNode{
					parent:    node,
					actionIdx: i,
					budget:    node.budget - p.Overhead,
					runMask:   node.runMask | (1 << uint(i)),
				}
				node.children = append(node.children, child)
			}
		}

		// Simulation: random rollout from node.
		simBudget := node.budget
		simMask := node.runMask
		reward := 0.0
		// Gather candidate indices.
		var cands []int
		for i, p := range probes {
			if simMask&(1<<uint(i)) == 0 && p.Overhead <= simBudget {
				cands = append(cands, i)
			}
		}
		localRng.Shuffle(len(cands), func(a, b int) { cands[a], cands[b] = cands[b], cands[a] })
		for _, i := range cands {
			p := probes[i]
			if p.Overhead > simBudget {
				continue
			}
			simBudget -= p.Overhead
			simMask |= 1 << uint(i)
			reward += p.Utility
		}

		// Backpropagation.
		cur := node
		for cur != nil {
			cur.visits++
			cur.value += reward
			cur = cur.parent
		}
	}

	// Extract the best action sequence greedily from root children.
	type entry struct {
		idx   int
		value float64
	}
	var ranked []entry
	for _, ch := range root.children {
		if ch.visits > 0 {
			ranked = append(ranked, entry{ch.actionIdx, ch.value / ch.visits})
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].value > ranked[j].value
	})

	var selected []ProbeSpec
	remaining := budget
	var usedMask uint64
	for _, e := range ranked {
		p := probes[e.idx]
		if usedMask&(1<<uint(e.idx)) != 0 {
			continue
		}
		if p.Overhead > remaining {
			continue
		}
		selected = append(selected, p)
		remaining -= p.Overhead
		usedMask |= 1 << uint(e.idx)
	}
	return selected
}
