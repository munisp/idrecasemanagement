// roles.go — role checks shared across the API.
package main

func hasRole(p principal, role string) bool {
	for _, rr := range p.Roles {
		if rr == role {
			return true
		}
		// Clinical role split: the legacy combined NURSE_PHYSICIAN grant is
		// satisfied by either of the two replacement roles, so every existing
		// RBAC check (24 call sites) keeps working while new staff are
		// granted DOCTOR or NURSE separately.
		if role == "NURSE_PHYSICIAN" && (rr == "DOCTOR" || rr == "NURSE") {
			return true
		}
		// Per-tenant super admin: every check for CASE_MANAGER also admits
		// TENANT_ADMIN, so a tenant's own admin gets full CASE_MANAGER-
		// equivalent operational access (cases, CRM, financial, QA, copilot,
		// reporting, own-tenant staff admin, ...) without touching every one
		// of the ~50 existing hasAnyRole call sites that name CASE_MANAGER
		// individually. This never grants cross-tenant access: TENANT_ADMIN
		// isn't PLATFORM_ADMIN/FEDERAL_ADMIN/SERVICE_WORKER, so tenancy()
		// still confines it to tenants in its own group claim, same as any
		// other non-federal role. The extra admin powers (program rules,
		// tenant label, graph admin) are wired in separately, each already
		// scoped by the same tenant-of-record.
		if role == "CASE_MANAGER" && rr == "TENANT_ADMIN" {
			return true
		}
	}
	return false
}

func maskEIN(ein string) string {
	if len(ein) < 4 {
		return "****"
	}
	return "*****" + ein[len(ein)-4:]
}
