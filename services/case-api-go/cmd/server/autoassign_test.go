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

func TestPickNext(t *testing.T) {
	ids := []string{"a", "b", "c"}
	none := map[string]int{}
	if got := pickNext(ids, map[string]int{"a": 6, "b": 4, "c": 5}, "b", ""); got != "b" {
		t.Errorf("the fewest open cases goes first, even right after their turn: got %q", got)
	}
	if got := pickNext(ids, map[string]int{"a": 2, "b": 2, "c": 9}, "a", ""); got != "b" {
		t.Errorf("among equals, in turn: got %q", got)
	}
	if got := pickNext(ids, none, "a", "b"); got != "c" {
		t.Errorf("a hand-off passes over whoever has the case: got %q", got)
	}
	if got := pickNext(ids, map[string]int{"b": 0, "a": 3, "c": 3}, "", "b"); got != "a" {
		t.Errorf("the least loaded holds the case already: the next least loaded, in turn: got %q", got)
	}
	if got := pickNext([]string{"a"}, none, "", "a"); got != "a" {
		t.Errorf("the only one in the rotation: got %q", got)
	}
	if got := pickNext(nil, none, "", ""); got != "" {
		t.Errorf("nobody: got %q", got)
	}
	// Catching up: from 4/5/6, six cases bring everyone to 7 (simulated as each is given out).
	load, last := map[string]int{"a": 6, "b": 4, "c": 5}, ""
	for i := 0; i < 6; i++ {
		last = pickNext(ids, load, last, "")
		load[last]++
	}
	if load["a"] != 7 || load["b"] != 7 || load["c"] != 7 {
		t.Errorf("did not even out: %v", load)
	}
}

func TestRotationRole(t *testing.T) {
	if r, ok := rotationRole(""); r != "CASE_MANAGER" || !ok {
		t.Errorf("default: %q %v", r, ok)
	}
	if r, ok := rotationRole(" coder "); r != "CODER" || !ok {
		t.Errorf("coder: %q %v", r, ok)
	}
	if _, ok := rotationRole("PM"); ok {
		t.Error("a case is not assigned to the PM role")
	}
}

func TestRoleChanges(t *testing.T) {
	cur := []string{"default-roles-idre", "CASE_MANAGER", "offline_access"}
	add, remove := roleChanges(cur, []string{"CODER", "CASE_MANAGER", "CODER"})
	if len(add) != 1 || add[0] != "CODER" || len(remove) != 0 {
		t.Errorf("add a role: +%v -%v", add, remove)
	}
	add, remove = roleChanges(cur, []string{"ATTORNEY"})
	if len(add) != 1 || add[0] != "ATTORNEY" || len(remove) != 1 || remove[0] != "CASE_MANAGER" {
		t.Errorf("swap a role: +%v -%v", add, remove)
	}
	// Roles outside the staff set are never touched.
	_, remove = roleChanges([]string{"PLATFORM_ADMIN", "CODER"}, []string{"CODER"})
	if len(remove) != 0 {
		t.Errorf("a platform role was removed: %v", remove)
	}
}
