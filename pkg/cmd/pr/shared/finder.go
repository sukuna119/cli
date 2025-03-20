package shared

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cli/cli/v2/api"
	remotes "github.com/cli/cli/v2/context"
	"github.com/cli/cli/v2/git"
	fd "github.com/cli/cli/v2/internal/featuredetection"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/pkg/cmdutil"
	o "github.com/cli/cli/v2/pkg/option"
	"github.com/cli/cli/v2/pkg/set"
	"github.com/shurcooL/githubv4"
	"golang.org/x/sync/errgroup"
)

type PRFinder interface {
	Find(opts FindOptions) (*api.PullRequest, ghrepo.Interface, error)
}

type progressIndicator interface {
	StartProgressIndicator()
	StopProgressIndicator()
}

type gitConfigClient interface {
	ReadBranchConfig(ctx context.Context, branchName string) (git.BranchConfig, error)
	PushDefault(ctx context.Context) (git.PushDefault, error)
	RemotePushDefault(ctx context.Context) (string, error)
	PushRevision(ctx context.Context, branchName string) (git.RemoteTrackingRef, error)
}

type finder struct {
	baseRepoFn      func() (ghrepo.Interface, error)
	branchFn        func() (string, error)
	remotesFn       func() (remotes.Remotes, error)
	httpClient      func() (*http.Client, error)
	branchConfig    func(string) (git.BranchConfig, error)
	gitConfigClient gitConfigClient
	progress        progressIndicator

	baseRefRepo ghrepo.Interface
	prNumber    int
	branchName  string
}

func NewFinder(factory *cmdutil.Factory) PRFinder {
	if runCommandFinder != nil {
		f := runCommandFinder
		runCommandFinder = &mockFinder{err: errors.New("you must use a RunCommandFinder to stub PR lookups")}
		return f
	}

	return &finder{
		baseRepoFn:      factory.BaseRepo,
		branchFn:        factory.Branch,
		remotesFn:       factory.Remotes,
		httpClient:      factory.HttpClient,
		gitConfigClient: factory.GitClient,
		branchConfig: func(s string) (git.BranchConfig, error) {
			return factory.GitClient.ReadBranchConfig(context.Background(), s)
		},
		progress: factory.IOStreams,
	}
}

var runCommandFinder PRFinder

// RunCommandFinder is the NewMockFinder substitute to be used ONLY in runCommand-style tests.
func RunCommandFinder(selector string, pr *api.PullRequest, repo ghrepo.Interface) *mockFinder {
	finder := NewMockFinder(selector, pr, repo)
	runCommandFinder = finder
	return finder
}

type FindOptions struct {
	// Selector can be a number with optional `#` prefix, a branch name with optional `<owner>:` prefix, or
	// a PR URL.
	Selector string
	// Fields lists the GraphQL fields to fetch for the PullRequest.
	Fields []string
	// BaseBranch is the name of the base branch to scope the PR-for-branch lookup to.
	BaseBranch string
	// States lists the possible PR states to scope the PR-for-branch lookup to.
	States []string
}

// TODO: Does this also need the BaseBranchName?
// TODO: Should this also hold the MergeBase?
// PR's are represented by the following:
// headRef -----PR-----> baseRef
//
// A ref is described as "remoteName/branchName", so
// headRepoName/headBranchName -----PR-----> baseRepoName/baseBranchName
type PullRequestRefs struct {
	// Head       PullRequestRef
	// Base       PullRequestRef
	BranchName string
	HeadRepo   ghrepo.Interface
	BaseRepo   ghrepo.Interface
}

// type PullRequestRef struct {
// 	Branch string
// 	Repo   ghrepo.Interface
// }

func (s *PullRequestRefs) HasHead() bool {
	return s.HeadRepo != nil && s.BranchName != ""
}

// GetPRHeadLabel returns the string that the GitHub API uses to identify the PR. This is
// either just the branch name or, if the PR is originating from a fork, the fork owner
// and the branch name, like <user>:<branch>.
func (s *PullRequestRefs) GetPRHeadLabel() string {
	if ghrepo.IsSame(s.HeadRepo, s.BaseRepo) {
		return s.BranchName
	}
	return fmt.Sprintf("%s:%s", s.HeadRepo.RepoOwner(), s.BranchName)
}

func (f *finder) Find(opts FindOptions) (*api.PullRequest, ghrepo.Interface, error) {
	// If we have a URL, we don't need git stuff
	if len(opts.Fields) == 0 {
		return nil, nil, errors.New("Find error: no fields specified")
	}

	if repo, prNumber, err := f.parseURL(opts.Selector); err == nil {
		f.prNumber = prNumber
		f.baseRefRepo = repo
	}

	if f.baseRefRepo == nil {
		repo, err := f.baseRepoFn()
		if err != nil {
			return nil, nil, err
		}
		f.baseRefRepo = repo
	}

	var prRefs PullRequestRefs
	if opts.Selector == "" {
		// You must be in a git repo for this case to work
		currentBranchName, err := f.branchFn()
		if err != nil {
			return nil, nil, err
		}
		f.branchName = currentBranchName

		// Get the branch config for the current branchName
		branchConfig, err := f.gitConfigClient.ReadBranchConfig(context.Background(), f.branchName)
		if err != nil {
			return nil, nil, err
		}

		// Determine if the branch is configured to merge to a special PR ref
		prHeadRE := regexp.MustCompile(`^refs/pull/(\d+)/head$`)
		if m := prHeadRE.FindStringSubmatch(branchConfig.MergeRef); m != nil {
			prNumber, _ := strconv.Atoi(m[1])
			f.prNumber = prNumber
		}

		// Determine the PullRequestRefs from config
		if f.prNumber == 0 {
			rems, err := f.remotesFn()
			if err != nil {
				return nil, nil, err
			}

			prRefs, err = ResolvePRRefs(f.gitConfigClient, rems, f.baseRefRepo, f.branchName)
			if err != nil {
				return nil, nil, err
			}
		}

	} else if f.prNumber == 0 {
		// You gave me a selector but I couldn't find a PR number (it wasn't a URL)

		// Try to get a PR number from the selector
		prNumber, err := strconv.Atoi(strings.TrimPrefix(opts.Selector, "#"))
		// If opts.Selector is a valid number then assume it is the
		// PR number unless opts.BaseBranch is specified. This is a
		// special case for PR create command which will always want
		// to assume that a numerical selector is a branch name rather
		// than PR number.
		if opts.BaseBranch == "" && err == nil {
			f.prNumber = prNumber
		} else {
			f.branchName = opts.Selector
			prRefs = PullRequestRefs{
				BaseRepo:   f.baseRefRepo,
				HeadRepo:   f.baseRefRepo,
				BranchName: f.branchName,
			}
		}
	}

	// Set up HTTP client
	httpClient, err := f.httpClient()
	if err != nil {
		return nil, nil, err
	}

	// TODO(josebalius): Should we be guarding here?
	if f.progress != nil {
		f.progress.StartProgressIndicator()
		defer f.progress.StopProgressIndicator()
	}

	fields := set.NewStringSet()
	fields.AddValues(opts.Fields)
	numberFieldOnly := fields.Len() == 1 && fields.Contains("number")
	fields.AddValues([]string{"id", "number"}) // for additional preload queries below

	if fields.Contains("isInMergeQueue") || fields.Contains("isMergeQueueEnabled") {
		cachedClient := api.NewCachedHTTPClient(httpClient, time.Hour*24)
		detector := fd.NewDetector(cachedClient, f.baseRefRepo.RepoHost())
		prFeatures, err := detector.PullRequestFeatures()
		if err != nil {
			return nil, nil, err
		}
		if !prFeatures.MergeQueue {
			fields.Remove("isInMergeQueue")
			fields.Remove("isMergeQueueEnabled")
		}
	}

	var getProjectItems bool
	if fields.Contains("projectItems") {
		getProjectItems = true
		fields.Remove("projectItems")
	}

	var pr *api.PullRequest
	if f.prNumber > 0 {
		if numberFieldOnly {
			// avoid hitting the API if we already have all the information
			return &api.PullRequest{Number: f.prNumber}, f.baseRefRepo, nil
		}
		pr, err = findByNumber(httpClient, f.baseRefRepo, f.prNumber, fields.ToSlice())
		if err != nil {
			return pr, f.baseRefRepo, err
		}
	} else {
		pr, err = findForBranch(httpClient, f.baseRefRepo, opts.BaseBranch, prRefs.GetPRHeadLabel(), opts.States, fields.ToSlice())
		if err != nil {
			return pr, f.baseRefRepo, err
		}
	}

	g, _ := errgroup.WithContext(context.Background())
	if fields.Contains("reviews") {
		g.Go(func() error {
			return preloadPrReviews(httpClient, f.baseRefRepo, pr)
		})
	}
	if fields.Contains("comments") {
		g.Go(func() error {
			return preloadPrComments(httpClient, f.baseRefRepo, pr)
		})
	}
	if fields.Contains("statusCheckRollup") {
		g.Go(func() error {
			return preloadPrChecks(httpClient, f.baseRefRepo, pr)
		})
	}
	if getProjectItems {
		g.Go(func() error {
			apiClient := api.NewClientFromHTTP(httpClient)
			err := api.ProjectsV2ItemsForPullRequest(apiClient, f.baseRefRepo, pr)
			if err != nil && !api.ProjectsV2IgnorableError(err) {
				return err
			}
			return nil
		})
	}

	return pr, f.baseRefRepo, g.Wait()
}

var pullURLRE = regexp.MustCompile(`^/([^/]+)/([^/]+)/pull/(\d+)`)

func (f *finder) parseURL(prURL string) (ghrepo.Interface, int, error) {
	if prURL == "" {
		return nil, 0, fmt.Errorf("invalid URL: %q", prURL)
	}

	u, err := url.Parse(prURL)
	if err != nil {
		return nil, 0, err
	}

	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, 0, fmt.Errorf("invalid scheme: %s", u.Scheme)
	}

	m := pullURLRE.FindStringSubmatch(u.Path)
	if m == nil {
		return nil, 0, fmt.Errorf("not a pull request URL: %s", prURL)
	}

	repo := ghrepo.NewWithHost(m[1], m[2], u.Hostname())
	prNumber, _ := strconv.Atoi(m[3])
	return repo, prNumber, nil
}

// type gitRefsForPR struct {
// 	base git.RemoteTrackingRef
// 	head git.RemoteTrackingRef
// }

// type PRRefs struct {
// 	BasePRRef BasePRRef
// 	HeadPRRef HeadPRRef
// }

// type BasePRRef struct {
// 	Repo   ghrepo.Interface
// 	Branch o.Option[string]
// }

// type HeadPRRef struct {
// 	Repo   ghrepo.Interface
// 	Branch string
// }

type either[T1 any, T2 any] struct {
	left  o.Option[T1]
	right o.Option[T2]
}

func left[T1 any, T2 any](v T1) either[T1, T2] {
	return either[T1, T2]{left: o.Some(v)}
}

func right[T1 any, T2 any](v T2) either[T1, T2] {
	return either[T1, T2]{right: o.Some(v)}
}

func eitherMatch[T1 any, T2 any, R any](e either[T1, T2], leftFn func(T1) R, rightFn func(T2) R) R {
	if v, present := e.left.Value(); present {
		return leftFn(v)
	}
	if v, present := e.right.Value(); present {
		return rightFn(v)
	}
	panic("eitherMatch: neither value present")
}

type pushLocation struct {
	remote     either[string, *url.URL]
	branchName string
}

func pushLocationForBranch(gitClient gitConfigClient, branch string) (o.Option[pushLocation], error) {
	// If @{push} resolves, then we have the remote tracking branch already, no problem.
	if pushRevisionRef, err := gitClient.PushRevision(context.Background(), branch); err == nil {
		return o.Some(pushLocation{
			remote:     left[string, *url.URL](pushRevisionRef.Remote),
			branchName: pushRevisionRef.Branch,
		}), nil
	}

	// But it doesn't always resolve, so we can suppress the error and move on to other means
	// of determination. We'll first look at branch and remote configuration to make a determination.
	// We start by assuming that BaseRepo and HeadRepo are the same, and the branch name is
	// the same as the local branch name, unless we find otherwise.
	branchConfig, err := gitClient.ReadBranchConfig(context.Background(), branch)
	if err != nil {
		return o.None[pushLocation](), err
	}

	pushDefault, err := gitClient.PushDefault(context.Background())
	if err != nil {
		return o.None[pushLocation](), err
	}

	// We assume the PR's branch name is the same as whatever was provided, unless the user has specified
	// push.default = upstream or tracking, then we use the branch name from the merge ref.
	remoteBranch := branch
	if pushDefault == git.PushDefaultUpstream || pushDefault == git.PushDefaultTracking {
		remoteBranch = strings.TrimPrefix(branchConfig.MergeRef, "refs/heads/")
	}

	// To get the remote, we look to the git config. It comes from one of the following, in order of precedence:
	// 1. branch.<name>.pushRemote
	// 2. remote.pushDefault
	// 3. branch.<name>.remote
	if branchConfig.PushRemoteName != "" {
		return o.Some(pushLocation{
			remote:     left[string, *url.URL](branchConfig.PushRemoteName),
			branchName: remoteBranch,
		}), nil
	}

	if branchConfig.PushRemoteURL != nil {
		return o.Some(pushLocation{
			remote:     right[string](branchConfig.PushRemoteURL),
			branchName: remoteBranch,
		}), nil
	}

	remotePushDefault, err := gitClient.RemotePushDefault(context.Background())
	if err != nil {
		return o.None[pushLocation](), err
	}

	if remotePushDefault != "" {
		return o.Some(pushLocation{
			remote:     left[string, *url.URL](remotePushDefault),
			branchName: remoteBranch,
		}), nil
	}

	if branchConfig.RemoteName != "" {
		return o.Some(pushLocation{
			remote:     left[string, *url.URL](branchConfig.RemoteName),
			branchName: remoteBranch,
		}), nil
	}

	if branchConfig.RemoteURL != nil {
		return o.Some(pushLocation{
			remote:     right[string](branchConfig.RemoteURL),
			branchName: remoteBranch,
		}), nil
	}

	// If we couldn't find the necessary information, indicate that with a None,
	// so that the caller can make a decision about how to proceed.
	return o.None[pushLocation](), nil
}

func GetPRRefs(gitClient gitConfigClient, remotes remotes.Remotes, branch string) (PullRequestRefs, error) {
	pushLocation, err := pushLocationForBranch(gitClient, branch)
	if err != nil {
		return PullRequestRefs{}, err
	}

	var remoteNameToRepo = func(remoteName string) ghrepo.Interface {
		remote, err := remotes.FindByName(remoteName)
		if err != nil {
			panic("TODO: make either smarter")
		}
		return remote.Repo
	}

	var remoteURLToRepo = func(remoteURL *url.URL) ghrepo.Interface {
		repo, err := ghrepo.FromURL(remoteURL)
		if err != nil {
			panic("TODO: make either smarter")
		}
		return repo
	}

	// If we have a push location, then use it to lookup the repo.
	if pushLocation, present := pushLocation.Value(); present {
		return PullRequestRefs{
			HeadRepo:   eitherMatch(pushLocation.remote, remoteNameToRepo, remoteURLToRepo),
			BranchName: pushLocation.branchName,
		}, nil

		// if remoteName, present := pushLocation.remoteName.Value(); present {
		// 	// If we have a remote, then use it to lookup the repo.
		// 	remote, err := remotes.FindByName(remoteName)
		// 	if err != nil {
		// 		return PullRequestRefs{}, err
		// 	}
		// 	return PullRequestRefs{
		// 		HeadRepo:   remote.Repo,
		// 		BranchName: pushLocation.branchName,
		// 	}, nil
		// }

		// if remoteURL, present := pushLocation.remoteURL.Value(); present {
		// 	// If we have a repo URL, then use it to lookup the repo.
		// 	repo, err := ghrepo.FromURL(remoteURL)
		// 	if err != nil {
		// 		return PullRequestRefs{}, fmt.Errorf("could not parse push remote URL %q: %w", remoteURL, err)
		// 	}
		// 	return PullRequestRefs{
		// 		HeadRepo:   repo,
		// 		BranchName: pushLocation.branchName,
		// 	}, nil
		// }

		// If we get here, something has gone wrong progammatically because a push location must have a remote or a remote URL.
		// Sadly, Go doesn't have sumtypes so it's not easy to make this a compile-time error.
		// TODO: Improve error message.
		// return PullRequestRefs{}, fmt.Errorf("push location must have a remote or remote URL, but got neither")
	}

	return PullRequestRefs{}, nil
}

func ResolvePRRefs(gitClient gitConfigClient, remotes remotes.Remotes, baseRepo ghrepo.Interface, localBranchName string) (PullRequestRefs, error) {
	// If @{push} resolves, then we have all the information we need to determine the head repo
	// and branch name. It is of the form <remote>/<branch>. We suppress the error here because
	// we have other means of computing the PullRequestRefs when this fails.
	if pushRevisionRef, err := gitClient.PushRevision(context.Background(), localBranchName); err == nil {
		remote, err := remotes.FindByName(pushRevisionRef.Remote)
		if err != nil {
			return PullRequestRefs{}, err
		}
		return PullRequestRefs{
			BaseRepo:   baseRepo,
			HeadRepo:   remote.Repo,
			BranchName: pushRevisionRef.Branch,
		}, nil
	}

	// Otherwise, we'll look at branch and remote configuration to make a determination.
	// We start by assuming that BaseRepo and HeadRepo are the same, and the branch name is
	// the same as the local branch name, unless we find otherwise.
	branchConfig, err := gitClient.ReadBranchConfig(context.Background(), localBranchName)
	if err != nil {
		return PullRequestRefs{}, err
	}

	pushDefault, err := gitClient.PushDefault(context.Background())
	if err != nil {
		return PullRequestRefs{}, err
	}

	// We assume the PR's branch name is the same as whatever was provided, unless the user has specified
	// push.default = upstream or tracking, then we use the branch name from the merge ref.
	remoteBranch := localBranchName
	if pushDefault == git.PushDefaultUpstream || pushDefault == git.PushDefaultTracking {
		remoteBranch = strings.TrimPrefix(branchConfig.MergeRef, "refs/heads/")
	}

	// To get the HeadRepo, we look to the git config. The HeadRepo comes from one of the following, in order of precedence:
	// 1. branch.<name>.pushRemote
	// 2. remote.pushDefault
	// 3. branch.<name>.remote
	if branchConfig.PushRemoteName != "" {
		r, err := remotes.FindByName(branchConfig.PushRemoteName)
		if err != nil {
			// TODO: make error include remotes
			return PullRequestRefs{}, fmt.Errorf("push remote %q not found: %w", branchConfig.PushRemoteName, err)
		}

		return PullRequestRefs{
			BaseRepo:   baseRepo,
			HeadRepo:   r.Repo,
			BranchName: remoteBranch,
		}, nil
	}

	if branchConfig.PushRemoteURL != nil {
		r, err := ghrepo.FromURL(branchConfig.PushRemoteURL)
		if err != nil {
			return PullRequestRefs{}, fmt.Errorf("could not parse push remote URL %q: %w", branchConfig.PushRemoteURL, err)
		}

		return PullRequestRefs{
			BaseRepo:   baseRepo,
			HeadRepo:   r,
			BranchName: remoteBranch,
		}, nil
	}

	remotePushDefault, err := gitClient.RemotePushDefault(context.Background())
	if err != nil {
		return PullRequestRefs{}, err
	}

	if remotePushDefault != "" {
		r, err := remotes.FindByName(remotePushDefault)
		if err != nil {
			// TODO: make error include remotes
			return PullRequestRefs{}, fmt.Errorf("remote %q not found: %w", branchConfig.RemoteName, err)
		}

		return PullRequestRefs{
			BaseRepo:   baseRepo,
			HeadRepo:   r.Repo,
			BranchName: remoteBranch,
		}, nil
	}

	if branchConfig.RemoteName != "" {
		r, err := remotes.FindByName(branchConfig.RemoteName)
		if err != nil {
			// TODO: make error include remotes
			return PullRequestRefs{}, fmt.Errorf("remote %q not found: %w", branchConfig.RemoteName, err)
		}

		return PullRequestRefs{
			BaseRepo:   baseRepo,
			HeadRepo:   r.Repo,
			BranchName: remoteBranch,
		}, nil
	}

	if branchConfig.RemoteURL != nil {
		r, err := ghrepo.FromURL(branchConfig.RemoteURL)
		if err != nil {
			return PullRequestRefs{}, fmt.Errorf("could not parse remote URL %q: %w", branchConfig.RemoteURL, err)
		}
		return PullRequestRefs{
			BaseRepo:   baseRepo,
			HeadRepo:   r,
			BranchName: remoteBranch,
		}, nil
	}

	// If nothing else worked, we assume the PR is in the same repo as the base branch
	return PullRequestRefs{
		BaseRepo:   baseRepo,
		HeadRepo:   baseRepo,
		BranchName: remoteBranch,
	}, nil
}

func findByNumber(httpClient *http.Client, repo ghrepo.Interface, number int, fields []string) (*api.PullRequest, error) {
	type response struct {
		Repository struct {
			PullRequest api.PullRequest
		}
	}

	query := fmt.Sprintf(`
	query PullRequestByNumber($owner: String!, $repo: String!, $pr_number: Int!) {
		repository(owner: $owner, name: $repo) {
			pullRequest(number: $pr_number) {%s}
		}
	}`, api.PullRequestGraphQL(fields))

	variables := map[string]interface{}{
		"owner":     repo.RepoOwner(),
		"repo":      repo.RepoName(),
		"pr_number": number,
	}

	var resp response
	client := api.NewClientFromHTTP(httpClient)
	err := client.GraphQL(repo.RepoHost(), query, variables, &resp)
	if err != nil {
		return nil, err
	}

	return &resp.Repository.PullRequest, nil
}

func findForBranch(httpClient *http.Client, repo ghrepo.Interface, baseBranch, headBranchWithOwnerIfFork string, stateFilters, fields []string) (*api.PullRequest, error) {
	type response struct {
		Repository struct {
			PullRequests struct {
				Nodes []api.PullRequest
			}
			DefaultBranchRef struct {
				Name string
			}
		}
	}

	fieldSet := set.NewStringSet()
	fieldSet.AddValues(fields)
	// these fields are required for filtering below
	fieldSet.AddValues([]string{"state", "baseRefName", "headRefName", "isCrossRepository", "headRepositoryOwner"})

	query := fmt.Sprintf(`
	query PullRequestForBranch($owner: String!, $repo: String!, $headRefName: String!, $states: [PullRequestState!]) {
		repository(owner: $owner, name: $repo) {
			pullRequests(headRefName: $headRefName, states: $states, first: 30, orderBy: { field: CREATED_AT, direction: DESC }) {
				nodes {%s}
			}
			defaultBranchRef { name }
		}
	}`, api.PullRequestGraphQL(fieldSet.ToSlice()))

	branchWithoutOwner := headBranchWithOwnerIfFork
	if idx := strings.Index(headBranchWithOwnerIfFork, ":"); idx >= 0 {
		branchWithoutOwner = headBranchWithOwnerIfFork[idx+1:]
	}

	variables := map[string]interface{}{
		"owner":       repo.RepoOwner(),
		"repo":        repo.RepoName(),
		"headRefName": branchWithoutOwner,
		"states":      stateFilters,
	}

	var resp response
	client := api.NewClientFromHTTP(httpClient)
	err := client.GraphQL(repo.RepoHost(), query, variables, &resp)
	if err != nil {
		return nil, err
	}

	prs := resp.Repository.PullRequests.Nodes
	sort.SliceStable(prs, func(a, b int) bool {
		return prs[a].State == "OPEN" && prs[b].State != "OPEN"
	})

	for _, pr := range prs {
		headBranchMatches := pr.HeadLabel() == headBranchWithOwnerIfFork
		baseBranchEmptyOrMatches := baseBranch == "" || pr.BaseRefName == baseBranch
		// When the head is the default branch, it doesn't really make sense to show merged or closed PRs.
		// https://github.com/cli/cli/issues/4263
		isNotClosedOrMergedWhenHeadIsDefault := pr.State == "OPEN" || resp.Repository.DefaultBranchRef.Name != headBranchWithOwnerIfFork
		if headBranchMatches && baseBranchEmptyOrMatches && isNotClosedOrMergedWhenHeadIsDefault {
			return &pr, nil
		}
	}

	return nil, &NotFoundError{fmt.Errorf("no pull requests found for branch %q", headBranchWithOwnerIfFork)}
}

func preloadPrReviews(httpClient *http.Client, repo ghrepo.Interface, pr *api.PullRequest) error {
	if !pr.Reviews.PageInfo.HasNextPage {
		return nil
	}

	type response struct {
		Node struct {
			PullRequest struct {
				Reviews api.PullRequestReviews `graphql:"reviews(first: 100, after: $endCursor)"`
			} `graphql:"...on PullRequest"`
		} `graphql:"node(id: $id)"`
	}

	variables := map[string]interface{}{
		"id":        githubv4.ID(pr.ID),
		"endCursor": githubv4.String(pr.Reviews.PageInfo.EndCursor),
	}

	gql := api.NewClientFromHTTP(httpClient)

	for {
		var query response
		err := gql.Query(repo.RepoHost(), "ReviewsForPullRequest", &query, variables)
		if err != nil {
			return err
		}

		pr.Reviews.Nodes = append(pr.Reviews.Nodes, query.Node.PullRequest.Reviews.Nodes...)
		pr.Reviews.TotalCount = len(pr.Reviews.Nodes)

		if !query.Node.PullRequest.Reviews.PageInfo.HasNextPage {
			break
		}
		variables["endCursor"] = githubv4.String(query.Node.PullRequest.Reviews.PageInfo.EndCursor)
	}

	pr.Reviews.PageInfo.HasNextPage = false
	return nil
}

func preloadPrComments(client *http.Client, repo ghrepo.Interface, pr *api.PullRequest) error {
	if !pr.Comments.PageInfo.HasNextPage {
		return nil
	}

	type response struct {
		Node struct {
			PullRequest struct {
				Comments api.Comments `graphql:"comments(first: 100, after: $endCursor)"`
			} `graphql:"...on PullRequest"`
		} `graphql:"node(id: $id)"`
	}

	variables := map[string]interface{}{
		"id":        githubv4.ID(pr.ID),
		"endCursor": githubv4.String(pr.Comments.PageInfo.EndCursor),
	}

	gql := api.NewClientFromHTTP(client)

	for {
		var query response
		err := gql.Query(repo.RepoHost(), "CommentsForPullRequest", &query, variables)
		if err != nil {
			return err
		}

		pr.Comments.Nodes = append(pr.Comments.Nodes, query.Node.PullRequest.Comments.Nodes...)
		pr.Comments.TotalCount = len(pr.Comments.Nodes)

		if !query.Node.PullRequest.Comments.PageInfo.HasNextPage {
			break
		}
		variables["endCursor"] = githubv4.String(query.Node.PullRequest.Comments.PageInfo.EndCursor)
	}

	pr.Comments.PageInfo.HasNextPage = false
	return nil
}

func preloadPrChecks(client *http.Client, repo ghrepo.Interface, pr *api.PullRequest) error {
	if len(pr.StatusCheckRollup.Nodes) == 0 {
		return nil
	}
	statusCheckRollup := &pr.StatusCheckRollup.Nodes[0].Commit.StatusCheckRollup.Contexts
	if !statusCheckRollup.PageInfo.HasNextPage {
		return nil
	}

	endCursor := statusCheckRollup.PageInfo.EndCursor

	type response struct {
		Node *api.PullRequest
	}

	query := fmt.Sprintf(`
	query PullRequestStatusChecks($id: ID!, $endCursor: String!) {
		node(id: $id) {
			...on PullRequest {
				%s
			}
		}
	}`, api.StatusCheckRollupGraphQLWithoutCountByState("$endCursor"))

	variables := map[string]interface{}{
		"id": pr.ID,
	}

	apiClient := api.NewClientFromHTTP(client)
	for {
		variables["endCursor"] = endCursor
		var resp response
		err := apiClient.GraphQL(repo.RepoHost(), query, variables, &resp)
		if err != nil {
			return err
		}

		result := resp.Node.StatusCheckRollup.Nodes[0].Commit.StatusCheckRollup.Contexts
		statusCheckRollup.Nodes = append(
			statusCheckRollup.Nodes,
			result.Nodes...,
		)

		if !result.PageInfo.HasNextPage {
			break
		}
		endCursor = result.PageInfo.EndCursor
	}

	statusCheckRollup.PageInfo.HasNextPage = false
	return nil
}

type NotFoundError struct {
	error
}

func (err *NotFoundError) Unwrap() error {
	return err.error
}

func NewMockFinder(selector string, pr *api.PullRequest, repo ghrepo.Interface) *mockFinder {
	var err error
	if pr == nil {
		err = &NotFoundError{errors.New("no pull requests found")}
	}
	return &mockFinder{
		expectSelector: selector,
		pr:             pr,
		repo:           repo,
		err:            err,
	}
}

type mockFinder struct {
	called         bool
	expectSelector string
	expectFields   []string
	pr             *api.PullRequest
	repo           ghrepo.Interface
	err            error
}

func (m *mockFinder) Find(opts FindOptions) (*api.PullRequest, ghrepo.Interface, error) {
	if m.err != nil {
		return nil, nil, m.err
	}
	if m.expectSelector != opts.Selector {
		return nil, nil, fmt.Errorf("mockFinder: expected selector %q, got %q", m.expectSelector, opts.Selector)
	}
	if len(m.expectFields) > 0 && !isEqualSet(m.expectFields, opts.Fields) {
		return nil, nil, fmt.Errorf("mockFinder: expected fields %v, got %v", m.expectFields, opts.Fields)
	}
	if m.called {
		return nil, nil, errors.New("mockFinder used more than once")
	}
	m.called = true

	if m.pr.HeadRepositoryOwner.Login == "" {
		// pose as same-repo PR by default
		m.pr.HeadRepositoryOwner.Login = m.repo.RepoOwner()
	}

	return m.pr, m.repo, nil
}

func (m *mockFinder) ExpectFields(fields []string) {
	m.expectFields = fields
}

func isEqualSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	aCopy := make([]string, len(a))
	copy(aCopy, a)
	bCopy := make([]string, len(b))
	copy(bCopy, b)
	sort.Strings(aCopy)
	sort.Strings(bCopy)

	for i := range aCopy {
		if aCopy[i] != bCopy[i] {
			return false
		}
	}
	return true
}
