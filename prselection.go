package main

// PRSelection is the single ranking result used by ticker and view adapters.
// Candidates are source-ordered: own/current branch, then newest notes first.
// OPEN beats MERGED beats CLOSED/unknown; latest merge wins; other ties retain
// source order. A whole PR is retained, including local policy/gate evidence.
type PRSelection struct {
	PR     *PR
	Merged []PR
	Open   int
}

func selectPR(candidates []PR) PRSelection {
	var s PRSelection
	seen := map[string]bool{}
	for _, pr := range candidates {
		if pr.URL != "" && seen[pr.URL] {
			continue
		}
		seen[pr.URL] = true
		if s.PR == nil || prRank(pr, *s.PR) {
			s.PR = &pr
		}
		if pr.State == "OPEN" {
			s.Open++
		}
		if pr.State == "MERGED" {
			s.Merged = append(s.Merged, pr)
		}
	}
	return s
}
