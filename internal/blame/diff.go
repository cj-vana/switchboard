package blame

// align returns, for every line of b, the index of the line of a it pairs
// with in a longest common subsequence, or -1 for a line a does not
// explain. Attribution rides these pairs: a paired line keeps its origin,
// an unpaired one belongs to whatever produced b.
//
// Common prefix and suffix are paired first, which is most of any real
// file after an edit; the middle falls to an LCS table. A middle too
// large for the table — two versions sharing almost nothing, at size —
// pairs nothing, which degrades attribution toward "this rewrite wrote
// it" and never toward a guess.

const maxCells = 4 << 20

func align(a, b []string) []int {
	out := make([]int, len(b))
	for i := range out {
		out[i] = -1
	}
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		out[prefix] = prefix
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		out[len(b)-1-suffix] = len(a) - 1 - suffix
		suffix++
	}
	ma, mb := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]
	n, m := len(ma), len(mb)
	if n == 0 || m == 0 || n*m > maxCells {
		return out
	}

	// dp[i][j] is the LCS length of ma[i:] and mb[j:], flattened.
	dp := make([]int32, (n+1)*(m+1))
	at := func(i, j int) int { return i*(m+1) + j }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case ma[i] == mb[j]:
				dp[at(i, j)] = dp[at(i+1, j+1)] + 1
			case dp[at(i+1, j)] >= dp[at(i, j+1)]:
				dp[at(i, j)] = dp[at(i+1, j)]
			default:
				dp[at(i, j)] = dp[at(i, j+1)]
			}
		}
	}
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case ma[i] == mb[j]:
			out[prefix+j] = prefix + i
			i++
			j++
		case dp[at(i+1, j)] >= dp[at(i, j+1)]:
			i++
		default:
			j++
		}
	}
	return out
}
