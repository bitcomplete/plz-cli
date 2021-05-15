package actions

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitHTTP "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/google/go-github/v32/github"
	"github.com/pkg/errors"
)

// gitHubRepo composes a local Git repository that is cloned from a GitHub repo.
type gitHubRepo struct {
	gitHubClient     *github.Client
	gitRepo          *git.Repository
	gitAuth          transport.AuthMethod
	gitHubRepo       *github.Repository
	defaultBranchRef *plumbing.Reference
}

func newGitHubRepo(ctx context.Context, authToken string) (*gitHubRepo, error) {
	// Initialize clients and Git repo.
	httpClient := &http.Client{
		Transport: &authTransport{Token: authToken},
	}
	gitHubClient := github.NewClient(httpClient)
	gitRepo, err := git.PlainOpenWithOptions(".", &git.PlainOpenOptions{
		DetectDotGit: true,
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Get the default branch from the GitHub repo
	proto, owner, repoName, err := parseRemote(gitRepo)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	var gitAuth transport.AuthMethod
	if proto == "https" {
		gitAuth = &gitHTTP.BasicAuth{Username: authToken}
	}
	ghRepo, _, err := gitHubClient.Repositories.Get(ctx, owner, repoName)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defaultBranchRefName := plumbing.NewRemoteReferenceName(
		git.DefaultRemoteName,
		ghRepo.GetDefaultBranch(),
	)
	defaultBranchRef, err := gitRepo.Reference(defaultBranchRefName, true)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return &gitHubRepo{
		gitHubClient:     gitHubClient,
		gitRepo:          gitRepo,
		gitAuth:          gitAuth,
		gitHubRepo:       ghRepo,
		defaultBranchRef: defaultBranchRef,
	}, nil
}

func (r *gitHubRepo) Client() *github.Client {
	return r.gitHubClient
}

func (r *gitHubRepo) GitRepo() *git.Repository {
	return r.gitRepo
}

func (r *gitHubRepo) GitAuth() transport.AuthMethod {
	return r.gitAuth
}

func (r *gitHubRepo) Owner() string {
	return r.gitHubRepo.Owner.GetLogin()
}

func (r *gitHubRepo) Name() string {
	return r.gitHubRepo.GetName()
}

func (r *gitHubRepo) DefaultBranch() string {
	return r.gitHubRepo.GetDefaultBranch()
}

func (r *gitHubRepo) DefaultBranchRef() *plumbing.Reference {
	return r.defaultBranchRef
}

type authTransport struct {
	http.Transport
	Token string
}

func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Add("Authorization", "token "+t.Token)
	return t.Transport.RoundTrip(r)
}

func parseRemote(repo *git.Repository) (string, string, string, error) {
	remote, err := repo.Remote(git.DefaultRemoteName)
	if err != nil {
		return "", "", "", err
	}
	remoteURL := remote.Config().URLs[0]
	path := ""
	proto := ""
	if strings.HasPrefix(remoteURL, "git@github.com:") {
		path = strings.TrimPrefix(remoteURL, "git@github.com:")
		proto = "ssh"
	} else if strings.HasPrefix(remoteURL, "https://github.com/") {
		path = strings.TrimPrefix(remoteURL, "https://github.com/")
		proto = "https"
	}
	pathFragments := strings.SplitN(path, "/", 2)
	if len(pathFragments) != 2 || proto == "" {
		return "", "", "", fmt.Errorf("remote url not well formed: %v", path)
	}
	owner := pathFragments[0]
	repoName := strings.TrimSuffix(pathFragments[1], ".git")
	return proto, owner, repoName, nil
}
