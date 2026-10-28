package main

import (
	"context"
	"errors"
)

// permissions are the privileges of the token per ACL path, as listed by
// /access/permissions; the value tells whether a privilege propagates to the
// paths below. PBS lists the effective privileges of each path, inherited
// ones included.
type permissions map[string]map[string]bool

// getPermissions returns the privileges of the token, read once per scrape.
func (s *scrapeState) getPermissions(ctx context.Context) (permissions, error) {
	return s.permissions.get(func() (permissions, error) {
		var out permissions
		err := s.api.get(ctx, apiPath("access", "permissions"), nil, &out)
		return out, err
	})
}

// has reports whether the token has priv on path itself.
func (p permissions) has(path, priv string) bool {
	_, ok := p[path][priv]
	return ok
}

// propagates reports whether the token has priv on path and the paths below.
func (p permissions) propagates(path, priv string) bool {
	return p[path][priv]
}

// canAuditTasks reports whether the token sees all tasks: Sys.Audit on
// /system/tasks itself, or propagated from /system (or "/").
func (p permissions) canAuditTasks() bool {
	return p.has("/system/tasks", "Sys.Audit") || p.propagates("/system", "Sys.Audit")
}

// canAuditUsers reports whether the token sees all users: Sys.Audit on
// /access/users itself, or propagated from /access (or "/").
func (p permissions) canAuditUsers() bool {
	return p.has("/access/users", "Sys.Audit") || p.propagates("/access", "Sys.Audit")
}

// hasRemoteAudit reports whether the token has Remote.Audit on /remote and
// it propagates to the remotes below (also when granted on "/"), so that
// /admin/sync lists the jobs of all remotes. Remote.Audit on a single remote
// (/remote/<name>) only shows the jobs of that remote.
func (p permissions) hasRemoteAudit() bool {
	return p.propagates("/remote", "Remote.Audit")
}

// canAuditDatastores reports whether the token sees every datastore with
// all its namespaces and jobs: Datastore.Audit on /datastore (or "/") that
// propagates.
func (p permissions) canAuditDatastores() bool {
	return p.propagates("/datastore", "Datastore.Audit")
}

// canReadSubscription reports whether PBS reports the subscription details
// (product name, due date) to the token: Sys.Audit on "/" itself.
func (p permissions) canReadSubscription() bool {
	return p.has("/", "Sys.Audit")
}

// errDatastoresFiltered is returned when the token lacks Datastore.Audit on
// /datastore: PBS then lists only the datastores, namespaces and jobs the
// token can audit, without an error.
var errDatastoresFiltered = errors.New("missing privilege Datastore.Audit on /datastore: datastores, namespaces and jobs the token can not audit are not listed")

// errSubscriptionFiltered is returned when the token lacks Sys.Audit on "/":
// PBS then leaves out the product name and due date of the subscription,
// without an error.
var errSubscriptionFiltered = errors.New("missing privilege Sys.Audit on /: the subscription is listed without product name and due date")

// privilegeGap returns missing if the permissions of the token fail ok: PBS
// then silently lists less instead of refusing the request. If the
// permissions can not be read, a complete list can not be told apart from an
// incomplete one, and that error is returned.
func (s *scrapeState) privilegeGap(ctx context.Context, ok func(permissions) bool, missing error) error {
	perms, err := s.getPermissions(ctx)
	switch {
	case err != nil:
		return err
	case !ok(perms):
		return missing
	}
	return nil
}
