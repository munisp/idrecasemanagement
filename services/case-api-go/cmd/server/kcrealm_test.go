package main

import "testing"

func TestKcRealmPath(t *testing.T) {
	dev := &server{cfg: Config{KeycloakIssuer: "https://keycloak-servers.newfire.app/realms/idre"}}
	prod := &server{cfg: Config{KeycloakIssuer: "https://keycloak-servers.newfire.app/realms/idre-prod/"}}
	none := &server{cfg: Config{}}
	cases := []struct {
		s          *server
		in, want   string
	}{
		{dev, "/admin/realms/idre/users", "/admin/realms/idre/users"},
		{prod, "/admin/realms/idre/users", "/admin/realms/idre-prod/users"},
		{prod, "/admin/realms/idre/group-by-path/tenant/fl", "/admin/realms/idre-prod/group-by-path/tenant/fl"},
		{prod, "/admin/realms/idre?fields=passwordPolicy", "/admin/realms/idre-prod?fields=passwordPolicy"},
		{prod, "/admin/realms/idre-prod/users", "/admin/realms/idre-prod/users"},
		{prod, "/realms/master/protocol/openid-connect/token", "/realms/master/protocol/openid-connect/token"},
		{none, "/admin/realms/idre/users", "/admin/realms/idre/users"},
	}
	for _, c := range cases {
		if got := c.s.kcRealmPath(c.in); got != c.want {
			t.Errorf("%s with issuer %q: got %s, want %s", c.in, c.s.cfg.KeycloakIssuer, got, c.want)
		}
	}
	if prod.kcRealm() != "idre-prod" || dev.kcRealm() != "idre" || none.kcRealm() != "idre" {
		t.Error("kcRealm wrong")
	}
}
