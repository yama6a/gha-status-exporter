package github

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// Repo is one repository from a listing.
type Repo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Fork          bool   `json:"fork"`
	Archived      bool   `json:"archived"`
	Disabled      bool   `json:"disabled"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Workflow is one workflow of a repository.
type Workflow struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

// Run is one workflow run.
type Run struct {
	Status         string    `json:"status"`
	Conclusion     string    `json:"conclusion"`
	HeadBranch     string    `json:"head_branch"`
	UpdatedAt      time.Time `json:"updated_at"`
	HeadRepository struct {
		ID int64 `json:"id"`
	} `json:"head_repository"`
}

// OwnerType returns "User" or "Organization".
func (c *Client) OwnerType(ctx context.Context, owner string) (string, error) {
	u, _, err := get[struct {
		Type string `json:"type"`
	}](ctx, c, fmt.Sprintf("%s/users/%s", c.base, url.PathEscape(owner)))
	return u.Type, err
}

// Repos lists the repos of a user the token owns, or of an org, across all pages.
func (c *Client) Repos(ctx context.Context, owner, ownerType string) ([]Repo, error) {
	next := fmt.Sprintf("%s/user/repos?affiliation=owner&per_page=100&sort=full_name", c.base)
	if ownerType == "Organization" {
		next = fmt.Sprintf("%s/orgs/%s/repos?per_page=100&sort=full_name", c.base, url.PathEscape(owner))
	}

	var all []Repo
	for next != "" {
		page, h, err := get[[]Repo](ctx, c, next)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		next = ""
		if m := nextLink.FindStringSubmatch(h.Get("Link")); m != nil {
			next = m[1]
		}
	}
	return all, nil
}

// Workflows lists the workflows of a repo.
func (c *Client) Workflows(ctx context.Context, owner, repo string) ([]Workflow, error) {
	res, _, err := get[struct {
		Workflows []Workflow `json:"workflows"`
	}](ctx, c, fmt.Sprintf("%s/repos/%s/%s/actions/workflows?per_page=100", c.base, owner, repo))
	return res.Workflows, err
}

// Runs returns the 20 newest completed runs of one workflow on a branch, newest first.
func (c *Client) Runs(ctx context.Context, owner, repo string, workflowID int64, branch string) ([]Run, error) {
	res, _, err := get[struct {
		Runs []Run `json:"workflow_runs"`
	}](ctx, c, fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%d/runs?branch=%s&status=completed&per_page=20",
		c.base, owner, repo, workflowID, url.QueryEscape(branch)))
	return res.Runs, err
}
