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

type PullRequestRef struct {
	Repo       ghrepo.Interface
	BranchName string
}

// TODO: Should this also hold the MergeBase?
// PR's are represented by the following:
// headRef -----PR-----> baseRef
//
// A ref is described as "remoteName/branchName", so
// headRepoName/headBranchName -----PR-----> baseRepoName/baseBranchName
type PullRequestRefs struct {
	HeadRef PullRequestRef
	BaseRef PullRequestRef
}

func (s *PullRequestRefs) HasHead() bool {
	return s.HeadRef.Repo != nil && s.HeadRef.BranchName != ""
}

// GetPRHeadLabel returns the string that the GitHub API uses to identify the PR. This is
// either just the branch name or, if the PR is originating from a fork, the fork owner
// and the branch name, like <user>:<branch>.
func (s *PullRequestRefs) GetPRHeadLabel() string {
	if ghrepo.IsSame(s.HeadRef.Repo, s.BaseRef.Repo) {
		return s.HeadRef.BranchName
	}
	return fmt.Sprintf("%s:%s", s.HeadRef.Repo.RepoOwner(), s.HeadRef.BranchName)
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

			prRefs, err = ResolvePullRequestRefs(f.gitConfigClient, rems, f.baseRefRepo, f.branchName)
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
				HeadRef: PullRequestRef{
					Repo:       f.baseRefRepo,
					BranchName: f.branchName,
				},
				BaseRef: PullRequestRef{
					Repo:       f.baseRefRepo,
					BranchName: f.branchName,
				},
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

type remote interface {
	toRepo(remotes remotes.Remotes) (ghrepo.Interface, error)
}

type remoteName struct {
	name string
}

func (rn remoteName) toRepo(remotes remotes.Remotes) (ghrepo.Interface, error) {
	remote, err := remotes.FindByName(rn.name)
	if err != nil {
		return nil, fmt.Errorf("could not find remote %q: %w", rn.name, err)
	}
	return remote.Repo, nil
}

type remoteURL struct {
	url *url.URL
}

func (ru remoteURL) toRepo(_ remotes.Remotes) (ghrepo.Interface, error) {
	repo, err := ghrepo.FromURL(ru.url)
	if err != nil {
		return nil, fmt.Errorf("could not parse remote URL %q: %w", ru.url, err)
	}
	return repo, nil
}

// A defaultPushTarget represents the remote name or URL and a branch name
// that we would expect a branch to be pushed to if `git push` were run with
// no further arguments. This is the most likely place for the head of the PR
// to be, but it's not guaranteed. The user may have pushed to another branch
// directly via `git push <remote> <local>:<remote>` and not set up tracking information.
// A branch name is always present.
//
// It's possible that we're unable to determine a remote, if the user had pushed directly
// to a URL for example `git push <url> <branch>`, which is why it is optional. When present,
// the remote may either be a name or a URL.
type defaultPushTarget struct {
	remote     o.Option[remote]
	branchName string
}

// newDefaultPushTarget is a thin wrapper over defaultPushTarget to help with
// generic type inference, to reduce verbosity in repeating the parametric type.
func newDefaultPushTarget(remote remote, branchName string) defaultPushTarget {
	return defaultPushTarget{
		remote:     o.Some(remote),
		branchName: branchName,
	}
}

// determineDefaultPushTarget uses git configuration to make a best guess about where a branch
// is pushed to, and where it would be pushed to if the user ran `git push` with no additional
// arguments.
//
// Firstly, it attempts to resolve the @{push} ref, which is the most reliable method, as this
// is what git uses to determine the remote tracking branch
//
// If this fails, we go through a series of steps to determine the remote, firstly by checking
// branch configuration which takes the form `branch.<name>.pushRemote = <name> | <url>`. If this is
// not set, then we check the remote configuration, which is `remote.pushDefault = <name>`. Finally,
// we check the branch configuration again for `branch.<name>.remote = <name> | <url>`.
// If none of these are set, we indicate that we were unable to determine the remote by returning
// a None value for the remote.
//
// The branch name is always set. The deafult configuration for push.default (current) indicates
// that a git push should use the same remote branch name as the local branch name. If push.default
// is set to upstream or tracking (deprecated form of upstream), then we use the branch name from the merge ref.
func determineDefaultPushTarget(gitClient gitConfigClient, branch string) (defaultPushTarget, error) {
	// If @{push} resolves, then we have the remote tracking branch already, no problem.
	if pushRevisionRef, err := gitClient.PushRevision(context.Background(), branch); err == nil {
		return newDefaultPushTarget(remoteName{pushRevisionRef.Remote}, pushRevisionRef.Branch), nil
	}

	// But it doesn't always resolve, so we can suppress the error and move on to other means
	// of determination. We'll first look at branch and remote configuration to make a determination.
	branchConfig, err := gitClient.ReadBranchConfig(context.Background(), branch)
	if err != nil {
		return defaultPushTarget{}, err
	}

	pushDefault, err := gitClient.PushDefault(context.Background())
	if err != nil {
		return defaultPushTarget{}, err
	}

	// We assume the PR's branch name is the same as whatever was provided, unless the user has specified
	// push.default = upstream or tracking, then we use the branch name from the merge ref.
	remoteBranch := branch
	if pushDefault == git.PushDefaultUpstream || pushDefault == git.PushDefaultTracking {
		remoteBranch = strings.TrimPrefix(branchConfig.MergeRef, "refs/heads/")
	}

	// To get the remote, we look to the git config. It comes from one of the following, in order of precedence:
	// 1. branch.<name>.pushRemote (which may be a name or a URL)
	// 2. remote.pushDefault (which is a remote name)
	// 3. branch.<name>.remote (which may be a name or a URL)
	if branchConfig.PushRemoteName != "" {
		return newDefaultPushTarget(
			remoteName{branchConfig.PushRemoteName},
			remoteBranch,
		), nil
	}

	if branchConfig.PushRemoteURL != nil {
		return newDefaultPushTarget(
			remoteURL{branchConfig.PushRemoteURL},
			remoteBranch,
		), nil
	}

	remotePushDefault, err := gitClient.RemotePushDefault(context.Background())
	if err != nil {
		return defaultPushTarget{}, err
	}

	if remotePushDefault != "" {
		return newDefaultPushTarget(
			remoteName{remotePushDefault},
			remoteBranch,
		), nil
	}

	if branchConfig.RemoteName != "" {
		return newDefaultPushTarget(
			remoteName{branchConfig.RemoteName},
			remoteBranch,
		), nil
	}

	if branchConfig.RemoteURL != nil {
		return newDefaultPushTarget(
			remoteURL{branchConfig.RemoteURL},
			remoteBranch,
		), nil
	}

	// If we couldn't find the remote, we'll indicate that to the caller via None.
	return defaultPushTarget{
		remote:     o.None[remote](),
		branchName: remoteBranch,
	}, nil
}

// defaultHeadPRRef is a neighbour to defaultPushTarget, but instead of holding
// basic git remote information, it holds a resolved repository in `gh` terms.
//
// Since we may not be able to determine a default remote for a branch, this
// is also true of the resolved repository.
type defaultHeadPRRef struct {
	repo       o.Option[ghrepo.Interface]
	branchName string
}

// resolveHeadPRRef is a thin wrapper around determineDefaultPushTarget, which attempts to convert
// a present remote into a resolved repository. If the remote is not present, we indicate that to the caller
// by returning a None value for the repo.
func resolveHeadPRRef(gitClient gitConfigClient, remotes remotes.Remotes, branch string) (defaultHeadPRRef, error) {
	pushTarget, err := determineDefaultPushTarget(gitClient, branch)
	if err != nil {
		return defaultHeadPRRef{}, err
	}

	// If we have no remote, let the caller decide what to do by indicating that with a None.
	if pushTarget.remote.IsNone() {
		return defaultHeadPRRef{
			repo:       o.None[ghrepo.Interface](),
			branchName: pushTarget.branchName,
		}, nil
	}

	repo, err := pushTarget.remote.Unwrap().toRepo(remotes)
	if err != nil {
		return defaultHeadPRRef{}, err
	}

	return defaultHeadPRRef{
		repo:       o.Some(repo),
		branchName: pushTarget.branchName,
	}, nil
}

func ResolvePullRequestRefs(gitClient gitConfigClient, remotes remotes.Remotes, baseRepo ghrepo.Interface, branch string) (PullRequestRefs, error) {
	headPRRef, err := resolveHeadPRRef(gitClient, remotes, branch)
	if err != nil {
		return PullRequestRefs{}, err
	}

	// If the repo was resolved, we can just convert the response
	// to a PullRequestRef and return it.
	if repo, present := headPRRef.repo.Value(); present {
		return PullRequestRefs{
			BaseRef: PullRequestRef{
				Repo:       baseRepo,
				BranchName: "", // we don't know it here? Perhaps a smell
			},
			HeadRef: PullRequestRef{
				Repo:       repo,
				BranchName: headPRRef.branchName,
			},
		}, nil
	}

	// If we don't have a remote, we need to make a decision about what to do.
	// For creation there is a "guess" step, but for everything else we anticipate the
	// head repo and base repo are the same.
	// TODO: guessing
	return PullRequestRefs{
		BaseRef: PullRequestRef{
			Repo:       baseRepo,
			BranchName: "", // we don't know it here? Perhaps a smell
		},
		HeadRef: PullRequestRef{
			Repo:       baseRepo, // <--- base repo
			BranchName: headPRRef.branchName,
		},
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
