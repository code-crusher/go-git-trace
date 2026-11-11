package transport

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
)

func linearHistory(t testing.TB) *canonicalRepo {
	t.Helper()
	r := newCanonicalRepo(t)
	r.commit("A")
	r.commit("B", "A")
	r.commit("C", "B")
	r.commit("D", "C")
	r.branch("main", "D")
	return r
}

func TestReproShallowServer(t *testing.T) {
	requireGitV2(t)
	r := linearHistory(t)
	require.NoError(t, r.st.SetShallow([]plumbing.Hash{r.commits["B"]}))

	args := func(name string, lines ...string) []string {
		out := make([]string, 0, len(lines))
		for _, l := range lines {
			f := strings.Fields(l)
			for i, w := range f {
				if h, ok := r.commits[w]; ok {
					f[i] = h.String()
				}
			}
			out = append(out, strings.Join(f, " "))
		}
		return out
	}

	cases := []struct {
		name  string
		lines []string
	}{
		{"no deepen", []string{"want D", "done"}},
		{"shallow D", []string{"want D", "shallow D", "done"}},
		{"shallow C", []string{"want D", "shallow C", "done"}},
		{"shallow D d1", []string{"want D", "shallow D", "deepen 1", "done"}},
		{"shallow D d2", []string{"want D", "shallow D", "deepen 2", "done"}},
		{"shallow C d100", []string{"want D", "shallow C", "deepen 100", "done"}},
		{"d1", []string{"want D", "deepen 1", "done"}},
		{"d2", []string{"want D", "deepen 2", "done"}},
		{"d3", []string{"want D", "deepen 3", "done"}},
		{"d100", []string{"want D", "deepen 100", "done"}},
		{"since C", []string{"want D", "deepen-since 0", "done"}},
	}
	for _, tc := range cases {
		a := args(tc.name, tc.lines...)
		req, err := io.ReadAll(v2Request(t, "fetch", nil, a))
		require.NoError(t, err)
		canonical := canonicalUploadPack(t, r, req, "GIT_PROTOCOL=version=2")
		got := serveUploadPackV2Test(t, r.st, io.NopCloser(bytes.NewReader(req)))
		sec := func(s string) string {
			i := strings.Index(s, "shallow-info")
			if i < 0 {
				return "(none)"
			}
			j := strings.Index(s, "packfile")
			return s[i:j]
		}
		t.Logf("%-14s git=%-60q go-git=%q", tc.name, sec(string(canonical)), sec(got))
	}
}
