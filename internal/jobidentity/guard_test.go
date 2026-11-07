package jobidentity

import "testing"

func TestJobIdentityFailsClosed(t *testing.T) {
	accepted := identity{current: "job_role", session: "job_role", limit: 6, login: true}
	if err := validate(accepted, "job_role", 6); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*identity){
		"wrong role":       func(r *identity) { r.current = "api_role" },
		"switched session": func(r *identity) { r.session = "admin" },
		"unlimited":        func(r *identity) { r.limit = -1 },
		"disabled":         func(r *identity) { r.limit = 0 },
		"over budget":      func(r *identity) { r.limit = 7 },
		"not login":        func(r *identity) { r.login = false },
		"superuser":        func(r *identity) { r.super = true },
		"create role":      func(r *identity) { r.createRole = true },
		"create database":  func(r *identity) { r.createDB = true },
		"replication":      func(r *identity) { r.replication = true },
		"bypass rls":       func(r *identity) { r.bypassRLS = true },
	} {
		t.Run(name, func(t *testing.T) {
			role := accepted
			change(&role)
			if validate(role, "job_role", 6) == nil {
				t.Fatal("unsafe role accepted")
			}
		})
	}
	accepted.limit = 1
	if validate(accepted, "job_role", 6) != nil {
		t.Fatal("stricter positive cap rejected")
	}
}
