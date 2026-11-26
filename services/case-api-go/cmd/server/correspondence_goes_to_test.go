package main

import (
	"reflect"
	"testing"
)

func TestGoesToFinalOrderRationaleCopy(t *testing.T) {
	all := []string{"all_parties"}
	for goes, want := range map[string][]string{"agency": {"agency"}, "provider": {"provider"}, "plan": {"health_plan"}, "": all} {
		if got := toRolesFor("final_order_rationale_copy", all, goes); !reflect.DeepEqual(got, want) {
			t.Errorf("copy to %q: %v, want %v", goes, got, want)
		}
	}
	if got := toRolesFor("rfi", nil, ""); !reflect.DeepEqual(got, []string{"provider"}) {
		t.Errorf("an RFI with no choice still goes to the provider: %v", got)
	}
	if got := toRolesFor("acceptance", []string{"provider"}, "agency"); !reflect.DeepEqual(got, []string{"provider"}) {
		t.Errorf("other templates keep their recipients: %v", got)
	}
}
