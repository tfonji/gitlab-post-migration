// Package membership temporarily grants the API token's own user Maintainer
// on the top-level groups an apply run touches, and takes it back afterwards.
//
// Why this exists: since GitLab 16.0, an instance admin's token is refused
// (403) on the protected branches API for any project the admin isn't a
// member of (gitlab-org/gitlab#428273) -- admin rights alone aren't enough.
// Migrated groups use SAML group links, so the service account is normally
// not a member. Group membership is inherited by every project and subgroup,
// so one grant per top-level group covers the whole run.
//
// Only what Ensure changed is reverted: a membership that already gave
// Maintainer or higher is left untouched, a lower one is restored to its
// previous level, and one Ensure created is removed.
package membership

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/tfonji/gitlab-post-migration/internal/gitlabclient"
)

// RequiredLevel is the minimum role that lets a user manage protected
// branches.
const RequiredLevel = gitlab.MaintainerPermissions

// Grant records one change Ensure made, so Revert can undo exactly that.
type Grant struct {
	GroupID   int64
	UserID    int64
	Username  string
	Added     bool                    // membership was created; revert removes it
	PrevLevel gitlab.AccessLevelValue // Added == false: level to restore
}

// Ensure makes the token user at least a Maintainer of each group. On error
// it returns the grants made so far, which the caller must still Revert.
func Ensure(ctx context.Context, c *gitlabclient.Client, groupIDs []int64) ([]Grant, error) {
	user, _, err := c.REST.Users.CurrentUser(gitlab.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("looking up token user: %w", err)
	}

	var grants []Grant
	for _, gid := range groupIDs {
		// members/all includes inherited and shared-group membership, so a
		// user who already has Maintainer by any route is left alone.
		if m, _, err := c.REST.GroupMembers.GetInheritedGroupMember(gid, user.ID, gitlab.WithContext(ctx)); err == nil && m.AccessLevel >= RequiredLevel {
			slog.Info("token user already has group access", "group_id", gid, "user", user.Username, "access_level", int(m.AccessLevel))
			continue
		} else if err != nil && !errors.Is(err, gitlab.ErrNotFound) {
			return grants, fmt.Errorf("checking %s's membership of group %d: %w", user.Username, gid, err)
		}

		direct, _, err := c.REST.GroupMembers.GetGroupMember(gid, user.ID, gitlab.WithContext(ctx))
		switch {
		case err == nil:
			if _, _, err := c.REST.GroupMembers.EditGroupMember(gid, user.ID, &gitlab.EditGroupMemberOptions{
				AccessLevel: gitlab.Ptr(RequiredLevel),
			}, gitlab.WithContext(ctx)); err != nil {
				return grants, fmt.Errorf("raising %s to Maintainer in group %d: %w", user.Username, gid, err)
			}
			grants = append(grants, Grant{GroupID: gid, UserID: user.ID, Username: user.Username, PrevLevel: direct.AccessLevel})
			slog.Info("raised token user to Maintainer for this run", "group_id", gid, "user", user.Username, "previous_level", int(direct.AccessLevel))
		case errors.Is(err, gitlab.ErrNotFound):
			// The expiry is a safety net: if the job is killed before
			// Revert runs, GitLab still drops the membership tomorrow.
			expires := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
			if _, _, err := c.REST.GroupMembers.AddGroupMember(gid, &gitlab.AddGroupMemberOptions{
				UserID:      gitlab.Ptr(user.ID),
				AccessLevel: gitlab.Ptr(RequiredLevel),
				ExpiresAt:   gitlab.Ptr(expires),
			}, gitlab.WithContext(ctx)); err != nil {
				return grants, fmt.Errorf("adding %s as Maintainer of group %d: %w", user.Username, gid, err)
			}
			grants = append(grants, Grant{GroupID: gid, UserID: user.ID, Username: user.Username, Added: true})
			slog.Info("added token user as Maintainer for this run", "group_id", gid, "user", user.Username, "expires_at", expires)
		default:
			return grants, fmt.Errorf("checking %s's direct membership of group %d: %w", user.Username, gid, err)
		}
	}
	return grants, nil
}

// Revert undoes grants in reverse order. It keeps going past failures so one
// bad group doesn't leave the others granted, and returns them joined.
func Revert(ctx context.Context, c *gitlabclient.Client, grants []Grant) error {
	var errs []error
	for i := len(grants) - 1; i >= 0; i-- {
		g := grants[i]
		if g.Added {
			if _, err := c.REST.GroupMembers.RemoveGroupMember(g.GroupID, g.UserID, nil, gitlab.WithContext(ctx)); err != nil {
				errs = append(errs, fmt.Errorf("removing %s from group %d: %w", g.Username, g.GroupID, err))
				continue
			}
			slog.Info("removed temporary group membership", "group_id", g.GroupID, "user", g.Username)
			continue
		}
		if _, _, err := c.REST.GroupMembers.EditGroupMember(g.GroupID, g.UserID, &gitlab.EditGroupMemberOptions{
			AccessLevel: gitlab.Ptr(g.PrevLevel),
		}, gitlab.WithContext(ctx)); err != nil {
			errs = append(errs, fmt.Errorf("restoring %s to level %d in group %d: %w", g.Username, g.PrevLevel, g.GroupID, err))
			continue
		}
		slog.Info("restored previous group access level", "group_id", g.GroupID, "user", g.Username, "access_level", int(g.PrevLevel))
	}
	return errors.Join(errs...)
}
