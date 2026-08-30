package devbox

import "testing"

func TestCheckDestroySafety(t *testing.T) {
	base := "0123456789012345678901234567890123456789"
	session := Session{BaseCommit: base, Branch: "devbox/project-01234567890123456789012345678901"}
	cases := []struct {
		name    string
		facts   RepositoryFacts
		wantErr bool
	}{
		{name: "dirty", facts: RepositoryFacts{Clean: false, Head: base}, wantErr: true},
		{name: "stash", facts: RepositoryFacts{Clean: true, Head: base, Stashed: true}, wantErr: true},
		{name: "other_branch", facts: RepositoryFacts{Clean: true, Head: base, OtherBranches: []string{"work"}}, wantErr: true},
		{name: "unexpected_ref", facts: RepositoryFacts{Clean: true, Head: base, UnexpectedRefs: []string{"refs/tags/work"}}, wantErr: true},
		{name: "reflog_commit", facts: RepositoryFacts{Clean: true, Head: base, UnpushedCommits: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, wantErr: true},
		{name: "unpushed", facts: RepositoryFacts{Clean: true, Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, wantErr: true},
		{name: "clean_base", facts: RepositoryFacts{Clean: true, Head: base}},
		{name: "remote_match", facts: RepositoryFacts{Clean: true, Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RemotePresent: true, RemoteHead: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		{name: "diverged", facts: RepositoryFacts{Clean: true, Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RemotePresent: true, RemoteHead: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := checkDestroySafety(session, test.facts)
			if got := err != nil; got != test.wantErr {
				t.Errorf("checkDestroySafety() returned error = %t, want %t; error: %v", got, test.wantErr, err)
			}
		})
	}
}
