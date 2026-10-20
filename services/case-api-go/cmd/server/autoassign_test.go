package main

import "testing"

func TestNextInRotation(t *testing.T) {
	ids := []string{"c", "a", "b"} // order given does not matter: the rotation is by id
	for _, c := range []struct{ last, want string }{
		{"", "a"}, {"a", "b"}, {"b", "c"}, {"c", "a"}, // in turn, wrapping round
		{"bb", "c"},  // the last one left the team: carry on from where they were
		{"zzz", "a"}, // past the end: start again
	} {
		if got := nextInRotation(ids, c.last); got != c.want {
			t.Errorf("after %q: got %q, want %q", c.last, got, c.want)
		}
	}
	if got := nextInRotation(nil, "a"); got != "" {
		t.Errorf("nobody to assign: got %q", got)
	}
	// Round robin is even: 9 cases over 3 Reviewers is 3 each.
	n, last := map[string]int{}, ""
	for i := 0; i < 9; i++ {
		last = nextInRotation(ids, last)
		n[last]++
	}
	if n["a"] != 3 || n["b"] != 3 || n["c"] != 3 {
		t.Errorf("uneven: %v", n)
	}
}

func TestInRotation(t *testing.T) {
	staff := []staffMember{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got := inRotation(staff, []string{"b"})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("taken out of the rotation: %v", got)
	}
	if len(inRotation(staff, nil)) != 3 {
		t.Error("nobody excluded: everyone is in")
	}
}
