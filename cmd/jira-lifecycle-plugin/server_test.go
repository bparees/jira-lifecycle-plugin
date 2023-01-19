package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/andygrunwald/go-jira"
	"github.com/google/go-cmp/cmp"
	"github.com/openshift-eng/jira-lifecycle-plugin/pkg/helpers"
	"github.com/openshift-eng/jira-lifecycle-plugin/pkg/labels"
	"github.com/sirupsen/logrus"
	"github.com/trivago/tgo/tcontainer"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	"k8s.io/test-infra/prow/bugzilla"
	prowconfig "k8s.io/test-infra/prow/config"
	cherrypicker "k8s.io/test-infra/prow/external-plugins/cherrypicker/lib"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/github/fakegithub"
	jiraclient "k8s.io/test-infra/prow/jira"
	"k8s.io/test-infra/prow/jira/fakejira"
	"k8s.io/test-infra/prow/pluginhelp"
)

var allowEventAndDate = cmp.AllowUnexported(event{}, jira.Date{})

type fakeGHClient struct {
	*fakegithub.FakeClient
}

func (f fakeGHClient) QueryWithGitHubAppsSupport(ctx context.Context, q interface{}, vars map[string]interface{}, org string) error {
	return nil
}

func TestHandle(t *testing.T) {
	t.Parallel()
	yes := true
	open := true
	v1Str := "v1"
	v2Str := "v2"
	v1 := []*jira.Version{{Name: v1Str}}
	v2 := []*jira.Version{{Name: v2Str}}
	v3 := []*jira.Version{{Name: "v3"}}
	updated := JiraBugState{Status: "UPDATED"}
	modified := JiraBugState{Status: "MODIFIED"}
	verified := []JiraBugState{{Status: "VERIFIED"}}
	jiraTransitions := []jira.Transition{
		{
			ID:   "1",
			Name: "NEW",
			To: jira.Status{
				Name: "NEW",
			},
		},
		{
			ID:   "2",
			Name: "MODIFIED",
			To: jira.Status{
				Name: "MODIFIED",
			},
		},
		{
			ID:   "3",
			Name: "UPDATED",
			To: jira.Status{
				Name: "UPDATED",
			},
		},
		{
			ID:   "4",
			Name: "VERIFIED",
			To: jira.Status{
				Name: "VERIFIED",
			},
		},
		{
			ID:   "5",
			Name: "CLOSED",
			To: jira.Status{
				Name: "CLOSED",
			},
		},
	}
	severityCritical := struct {
		Value string
	}{Value: "<img alt=\"\" src=\"/images/icons/priorities/critical.svg\" width=\"16\" height=\"16\"> Critical"}
	severityImportant := struct {
		Value string
	}{Value: "<img alt=\"\" src=\"/images/icons/priorities/important.svg\" width=\"16\" height=\"16\"> Important"}
	severityModerate := struct {
		Value string
	}{Value: "<img alt=\"\" src=\"/images/icons/priorities/moderate.svg\" width=\"16\" height=\"16\"> Moderate"}
	severityLow := struct {
		Value string
	}{Value: "<img alt=\"\" src=\"/images/icons/priorities/low.svg\" width=\"16\" height=\"16\"> Low"}
	cloneLinkTo123 := jira.IssueLink{
		Type: jira.IssueLinkType{
			Name:    "Cloners",
			Inward:  "is cloned by",
			Outward: "clones",
		},
		OutwardIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123"},
	}
	blocksLinkTo123 := jira.IssueLink{
		Type: jira.IssueLinkType{
			Name:    "Blocks",
			Inward:  "is blocked by",
			Outward: "blocks",
		},
		InwardIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123"},
	}
	// the fake clone doesn't include the key in the link, which breaks our check; just make a second struct without the key set
	cloneLinkTo123JustID := jira.IssueLink{
		Type: jira.IssueLinkType{
			Name:    "Cloners",
			Inward:  "is cloned by",
			Outward: "clones",
		},
		OutwardIssue: &jira.Issue{ID: "1"},
	}
	blocksLinkTo123JustID := jira.IssueLink{
		Type: jira.IssueLinkType{
			Name:    "Blocks",
			Inward:  "is blocked by",
			Outward: "blocks",
		},
		InwardIssue: &jira.Issue{ID: "1"},
	}
	cloneLinkTo124 := jira.IssueLink{
		Type: jira.IssueLinkType{
			Name:    "Cloners",
			Inward:  "is cloned by",
			Outward: "clone",
		},
		InwardIssue: &jira.Issue{ID: "2", Key: "OCPBUGS-124"},
	}
	blocksLinkTo124 := jira.IssueLink{
		Type: jira.IssueLinkType{
			Name:    "Blocks",
			Inward:  "is blocked by",
			Outward: "blocks",
		},
		OutwardIssue: &jira.Issue{ID: "2", Key: "OCPBUGS-124"},
	}
	cloneBetween123to124 := jira.IssueLink{
		Type: jira.IssueLinkType{
			Name:    "Cloners",
			Inward:  "is cloned by",
			Outward: "clones",
		},
		InwardIssue:  &jira.Issue{ID: "2", Key: "OCPBUGS-124"},
		OutwardIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123"},
	}
	blocksBetween123to124 := jira.IssueLink{
		Type: jira.IssueLinkType{
			Name:    "Blocks",
			Inward:  "is blocked by",
			Outward: "blocks",
		},
		OutwardIssue: &jira.Issue{ID: "2", Key: "OCPBUGS-124"},
		InwardIssue:  &jira.Issue{ID: "1", Key: "OCPBUGS-123"},
	}

	base := &event{
		org: "org", repo: "repo", baseRef: "branch", number: 1, key: "OCPBUGS-123", isBug: true, body: "This PR fixes OCPBUGS-123", title: "OCPBUGS-123: fixed it!", htmlUrl: "https://github.com/org/repo/pull/1", login: "user",
	}
	var testCases = []struct {
		name                       string
		labels                     []string
		humanLabelled              bool
		missing                    bool
		merged                     bool
		closed                     bool
		opened                     bool
		refresh                    bool
		cherrypick                 bool
		nonBug                     bool
		cherryPickFromPRNum        int
		body                       string
		title                      string
		key                        string
		remoteLinks                map[string][]jira.RemoteLink
		prs                        []github.PullRequest
		issues                     []jira.Issue
		issueGetErrors             map[string]error
		issueCreateErrors          map[string]error
		issueUpdateErrors          map[string]error
		options                    JiraBranchOptions
		expectedLabels             []string
		expectedComment            string
		expectedIssue              *jira.Issue
		expectedNewRemoteLinks     []jira.RemoteLink
		expectedRemovedRemoteLinks []jira.RemoteLink
		existingIssueLinks         []*jira.IssueLink
		// most of the tests can be handled by a single event struct with small modifications; for tests with more extensive differences, allow override
		overrideEvent          *event
		disabledProjects       []string
		expectedCommentUpdates []string
		bugs                   []bugzilla.Bug
		bugComments            map[int][]bugzilla.Comment
		bugSubComponents       map[int]map[string][]string
	}{
		{
			name:    "Unrelated event gets no action",
			body:    "this is a PR",
			title:   "this is a PR",
			missing: true,
			overrideEvent: &event{
				org: "org", repo: "repo", baseRef: "branch",
				number:  1,
				key:     "",
				htmlUrl: "https://github.com/org/repo/pull/1", login: "user",
			},
		},
		{
			name:    "title without key gets comment saying so on /jira refresh",
			body:    "/jira refresh",
			title:   "this is a PR",
			missing: true,
			refresh: true,
			overrideEvent: &event{
				org: "org", repo: "repo", baseRef: "branch",
				number: 1,
				body:   "/jira refresh", title: "this is a PR",
				htmlUrl: "https://github.com/org/repo/pull/1", login: "user",
			},
			expectedComment: `org/repo#1:@user: No Jira issue is referenced in the title of this pull request.
To reference a jira issue, add 'XYZ-NNN:' to the title of this pull request and request another refresh with <code>/jira refresh</code>.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>/jira refresh


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name: "no bug found leaves a comment",
			expectedComment: `org/repo#1:@user: No Jira issue with key OCPBUGS-123 exists in the tracker at https://my-jira.com.
Once a valid jira issue is referenced in the title of this pull request, request a refresh with <code>/jira refresh</code>.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:           "error fetching bug leaves a comment",
			issueGetErrors: map[string]error{"OCPBUGS-123": errors.New("injected error getting bug")},
			expectedComment: `org/repo#1:@user: An error was encountered searching for bug OCPBUGS-123 on the Jira server at https://my-jira.com. No known errors were detected, please see the full error message for details.

<details><summary>Full error message.</summary>

<code>
injected error getting bug
</code>

</details>

Please contact an administrator to resolve this issue, then request a bug refresh with <code>/jira refresh</code>.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:           "valid bug removes invalid label, adds valid/severity labels and comments",
			issues:         []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Unknowns: tcontainer.MarshalMap{helpers.SeverityField: severityCritical}}}},
			options:        JiraBranchOptions{}, // no requirements --> always valid
			labels:         []string{labels.JiraInvalidBug},
			expectedLabels: []string{labels.JiraValidRef, labels.JiraValidBug, labels.BugzillaValidBug, labels.SeverityCritical},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123), which is valid.

<details><summary>No validations were run on this bug</summary></details>

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:           "invalid bug adds invalid label, removes valid label and comments",
			issues:         []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Unknowns: tcontainer.MarshalMap{helpers.SeverityField: severityImportant}}}},
			options:        JiraBranchOptions{IsOpen: &open},
			labels:         []string{labels.JiraValidBug, labels.BugzillaValidBug, labels.SeverityCritical},
			expectedLabels: []string{labels.JiraValidRef, labels.JiraInvalidBug, labels.SeverityImportant},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123), which is invalid:
 - expected the bug to be open, but it isn't

Comment <code>/jira refresh</code> to re-evaluate validity if changes to the Jira bug are made, or edit the title of this pull request to link to a different bug.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:           "invalid bug adds keeps human-added valid bug label",
			issues:         []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Unknowns: tcontainer.MarshalMap{helpers.SeverityField: severityImportant}}}},
			options:        JiraBranchOptions{IsOpen: &open},
			humanLabelled:  true,
			labels:         []string{labels.JiraValidBug, labels.BugzillaValidBug, labels.SeverityCritical},
			expectedLabels: []string{labels.JiraValidRef, labels.JiraValidBug, labels.BugzillaValidBug, labels.SeverityImportant},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123), which is invalid:
 - expected the bug to be open, but it isn't

Comment <code>/jira refresh</code> to re-evaluate validity if changes to the Jira bug are made, or edit the title of this pull request to link to a different bug.

Retaining the jira/valid-bug label as it was manually added.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:    "no bug removes all labels and comments",
			missing: true,
			labels:  []string{labels.JiraValidBug, labels.BugzillaValidBug, labels.JiraInvalidBug},
			expectedComment: `org/repo#1:@user: No Jira issue is referenced in the title of this pull request.
To reference a jira issue, add 'XYZ-NNN:' to the title of this pull request and request another refresh with <code>/jira refresh</code>.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:   "valid bug with status update removes invalid label, adds valid label, comments and updates status",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Unknowns: tcontainer.MarshalMap{helpers.SeverityField: severityModerate}}}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			}},
			options:        JiraBranchOptions{StateAfterValidation: &updated}, // no requirements --> always valid
			labels:         []string{labels.JiraInvalidBug},
			expectedLabels: []string{labels.JiraValidRef, labels.JiraValidBug, labels.BugzillaValidBug, labels.SeverityModerate},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123), which is valid. The bug has been moved to the UPDATED state.

<details><summary>No validations were run on this bug</summary></details>

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status:   &jira.Status{Name: "UPDATED"},
				Unknowns: tcontainer.MarshalMap{helpers.SeverityField: severityModerate},
			}},
		},
		{
			name:           "valid jira removes invalid label, adds valid label, comments",
			key:            "JIRA-123",
			nonBug:         true,
			issues:         []jira.Issue{{ID: "1", Key: "JIRA-123", Fields: &jira.IssueFields{Unknowns: tcontainer.MarshalMap{helpers.SeverityField: severityModerate}}}},
			labels:         []string{labels.JiraInvalidBug},
			expectedLabels: []string{labels.JiraValidRef},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue JIRA-123](https://my-jira.com/browse/JIRA-123), which is a valid jira issue.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:           "invalid jira with status update removes valid label, comments",
			key:            "JIRA-123",
			nonBug:         true,
			labels:         []string{labels.JiraValidRef},
			expectedLabels: []string{},
			expectedComment: `org/repo#1:@user: No Jira issue with key JIRA-123 exists in the tracker at https://my-jira.com.
Once a valid jira issue is referenced in the title of this pull request, request a refresh with <code>/jira refresh</code>.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:           "valid no-jira removes invalid label, adds valid label, comments",
			key:            "NO-JIRA",
			nonBug:         true,
			labels:         []string{labels.JiraInvalidBug},
			expectedLabels: []string{labels.JiraValidRef},
			expectedComment: `org/repo#1:@user: This pull request explicitly references no jira issue.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:           "valid bug with status update removes invalid label, adds valid label, comments and updates status with resolution",
			issues:         []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Unknowns: tcontainer.MarshalMap{helpers.SeverityField: severityLow}}}},
			options:        JiraBranchOptions{StateAfterValidation: &JiraBugState{Status: "CLOSED", Resolution: "VALIDATED"}}, // no requirements --> always valid
			labels:         []string{labels.JiraInvalidBug},
			expectedLabels: []string{labels.JiraValidRef, labels.JiraValidBug, labels.BugzillaValidBug, labels.SeverityLow},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123), which is valid. The bug has been moved to the CLOSED (VALIDATED) state.

<details><summary>No validations were run on this bug</summary></details>

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{
					Name: "CLOSED",
				},
				Resolution: &jira.Resolution{
					Name: "VALIDATED",
				},
				// due to the way `Unknowns` works, this has to be provided as a map[string]interface{}
				Unknowns: tcontainer.MarshalMap{helpers.SeverityField: map[string]interface{}{"Value": string(`<img alt="" src="/images/icons/priorities/low.svg" width="16" height="16"> Low`)}},
			},
			},
		},
		{
			name:           "valid bug with status update removes invalid label, adds valid label, comments and does not update status when it is already correct",
			issues:         []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Status: &jira.Status{Name: "UPDATED"}}}},
			options:        JiraBranchOptions{StateAfterValidation: &updated}, // no requirements --> always valid
			labels:         []string{labels.JiraInvalidBug},
			expectedLabels: []string{labels.JiraValidRef, labels.JiraValidBug, labels.BugzillaValidBug},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123), which is valid.

<details><summary>No validations were run on this bug</summary></details>

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Status: &jira.Status{Name: "UPDATED"}}},
		},
		{
			name:           "valid bug with external link removes invalid label, adds valid label, comments, makes an external bug link",
			issues:         []jira.Issue{{ID: "1", Key: "OCPBUGS-123"}},
			options:        JiraBranchOptions{AddExternalLink: &yes}, // no requirements --> always valid
			labels:         []string{labels.JiraInvalidBug},
			expectedLabels: []string{labels.JiraValidRef, labels.JiraValidBug, labels.BugzillaValidBug},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123), which is valid.

<details><summary>No validations were run on this bug</summary></details>

The issue has been updated to refer to the pull request using the external issue tracker.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123"},
			expectedNewRemoteLinks: []jira.RemoteLink{{Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			},
			}},
		},
		{
			name:   "valid bug with already existing external link removes invalid label, adds valid label, comments to say nothing changed",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123"}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			}},
			options:        JiraBranchOptions{AddExternalLink: &yes}, // no requirements --> always valid
			labels:         []string{labels.JiraInvalidBug},
			expectedLabels: []string{labels.JiraValidRef, labels.JiraValidBug, labels.BugzillaValidBug},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123), which is valid.

<details><summary>No validations were run on this bug</summary></details>

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123"},
		},
		{
			name: "failure to fetch dependent bug results in a comment",
			issues: []jira.Issue{{ID: "2", Key: "OCPBUGS-124", Fields: &jira.IssueFields{
				IssueLinks: []*jira.IssueLink{&cloneLinkTo123, &blocksLinkTo123},
			}}},
			overrideEvent: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 2, key: "OCPBUGS-124", isBug: true, body: "This PR fixes OCPBUGS-124", title: "OCPBUGS-124: fixed it!", htmlUrl: "https://github.com/org/repo/pull/2", login: "user",
			},
			existingIssueLinks: []*jira.IssueLink{&cloneBetween123to124, &blocksBetween123to124},
			issueGetErrors:     map[string]error{"OCPBUGS-123": errors.New("injected error getting bug")},
			options:            JiraBranchOptions{DependentBugStates: &verified},
			expectedComment: `org/repo#2:@user: An error was encountered searching for dependent bug OCPBUGS-123 for bug OCPBUGS-124 on the Jira server at https://my-jira.com. No known errors were detected, please see the full error message for details.

<details><summary>Full error message.</summary>

<code>
injected error getting bug
</code>

</details>

Please contact an administrator to resolve this issue, then request a bug refresh with <code>/jira refresh</code>.

<details>

In response to [this](https://github.com/org/repo/pull/2):

>This PR fixes OCPBUGS-124


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name: "valid bug with dependent bugs removes invalid label, adds valid label, comments",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "VERIFIED"},
				IssueLinks: []*jira.IssueLink{&cloneLinkTo124, &blocksLinkTo124},
				Unknowns: tcontainer.MarshalMap{
					helpers.TargetVersionField: &v2,
				},
			},
			}, {ID: "2", Key: "OCPBUGS-124", Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "MODIFIED"},
				IssueLinks: []*jira.IssueLink{&cloneLinkTo123, &blocksLinkTo123},
				Unknowns: tcontainer.MarshalMap{
					helpers.TargetVersionField: &v1,
				},
			}}},
			overrideEvent: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 2, key: "OCPBUGS-124", isBug: true, body: "This PR fixes OCPBUGS-124", title: "OCPBUGS-124: fixed it!", htmlUrl: "https://github.com/org/repo/pull/2", login: "user",
			},
			existingIssueLinks: []*jira.IssueLink{&cloneBetween123to124, &blocksBetween123to124},
			options:            JiraBranchOptions{IsOpen: &yes, TargetVersion: &v1Str, DependentBugStates: &verified, DependentBugTargetVersions: &[]string{v2Str}},
			labels:             []string{labels.JiraInvalidBug},
			expectedLabels:     []string{labels.JiraValidRef, labels.JiraValidBug, labels.BugzillaValidBug},
			expectedComment: `org/repo#2:@user: This pull request references [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124), which is valid.

<details><summary>5 validation(s) were run on this bug</summary>

* bug is open, matching expected state (open)
* bug target version (v1) matches configured target version for branch (v1)
* dependent bug [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) is in the state VERIFIED, which is one of the valid states (VERIFIED)
* dependent [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) targets the "v2" version, which is one of the valid target versions: v2
* bug has dependents</details>

<details>

In response to [this](https://github.com/org/repo/pull/2):

>This PR fixes OCPBUGS-124


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:   "valid bug on merged PR with one external link migrates to new state with resolution and comments",
			merged: true,
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "MODIFIED"},
			}}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			}},
			prs:     []github.PullRequest{{Number: base.number, Merged: true}},
			options: JiraBranchOptions{StateAfterMerge: &JiraBugState{Status: "CLOSED", Resolution: "MERGED"}}, // no requirements --> always valid
			expectedComment: `org/repo#1:@user: All pull requests linked via external trackers have merged:
 * [org/repo#1](https://github.com/org/repo/pull/1)

[Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) has been moved to the CLOSED (MERGED) state.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "CLOSED"},
				Resolution: &jira.Resolution{Name: "MERGED"},
				Unknowns:   tcontainer.MarshalMap{},
			}},
		},
		{
			name:   "valid bug on merged PR with one external link migrates to new state and comments",
			merged: true,
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{}}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			}},
			prs:     []github.PullRequest{{Number: base.number, Merged: true}},
			options: JiraBranchOptions{StateAfterMerge: &modified}, // no requirements --> always valid
			expectedComment: `org/repo#1:@user: All pull requests linked via external trackers have merged:
 * [org/repo#1](https://github.com/org/repo/pull/1)

[Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) has been moved to the MODIFIED state.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Status: &jira.Status{Name: "MODIFIED"}}},
		},
		{
			name:   "valid bug on merged PR with many external links migrates to new state and comments",
			merged: true,
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{}}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{
				ID: 1,
				Object: &jira.RemoteLinkObject{
					URL:   "https://github.com/org/repo/pull/1",
					Title: "org/repo#1: OCPBUGS-123: fixed it!",
					Icon: &jira.RemoteLinkIcon{
						Url16x16: "https://github.com/favicon.ico",
						Title:    "GitHub",
					},
				},
			}, {
				ID: 2,
				Object: &jira.RemoteLinkObject{
					URL:   "https://github.com/org/repo/pull/22/commits/1234567890",
					Title: "org/repo#22: OCPBUGS-123: fixed it!",
					Icon: &jira.RemoteLinkIcon{
						Url16x16: "https://github.com/favicon.ico",
						Title:    "GitHub",
					},
				},
			}, {
				ID: 2,
				Object: &jira.RemoteLinkObject{
					URL:   "https://github.com/org/repo/pull/23/files",
					Title: "org/repo#23: OCPBUGS-123: fixed it!",
					Icon: &jira.RemoteLinkIcon{
						Url16x16: "https://github.com/favicon.ico",
						Title:    "GitHub",
					},
				},
			},
			}},
			prs:     []github.PullRequest{{Number: base.number, Merged: true}, {Number: 22, Merged: true}, {Number: 23, Merged: true}},
			options: JiraBranchOptions{StateAfterMerge: &modified}, // no requirements --> always valid
			expectedComment: `org/repo#1:@user: All pull requests linked via external trackers have merged:
 * [org/repo#1](https://github.com/org/repo/pull/1)
 * [org/repo#22](https://github.com/org/repo/pull/22)
 * [org/repo#23](https://github.com/org/repo/pull/23)

[Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) has been moved to the MODIFIED state.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Status: &jira.Status{Name: "MODIFIED"}}},
		},
		{
			name:   "valid bug on merged PR with unmerged external links does nothing",
			merged: true,
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{}}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{
				ID: 1,
				Object: &jira.RemoteLinkObject{
					URL:   "https://github.com/org/repo/pull/1",
					Title: "org/repo#1: OCPBUGS-123: fixed it!",
					Icon: &jira.RemoteLinkIcon{
						Url16x16: "https://github.com/favicon.ico",
						Title:    "GitHub",
					},
				},
			}, {
				ID: 2,
				Object: &jira.RemoteLinkObject{
					URL:   "https://github.com/org/repo/pull/22",
					Title: "org/repo#22: OCPBUGS-123: fixed it!",
					Icon: &jira.RemoteLinkIcon{
						Url16x16: "https://github.com/favicon.ico",
						Title:    "GitHub",
					},
				},
			},
			}},
			prs:           []github.PullRequest{{Number: base.number, Merged: true}, {Number: 22, Merged: false, State: "open"}},
			options:       JiraBranchOptions{StateAfterMerge: &modified}, // no requirements --> always valid
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{}},
			expectedComment: `org/repo#1:@user: Some pull requests linked via external trackers have merged:
 * [org/repo#1](https://github.com/org/repo/pull/1)

The following pull requests linked via external trackers have not merged:
 * [org/repo#22](https://github.com/org/repo/pull/22) is open

These pull request must merge or be unlinked from the Jira bug in order for it to move to the next state. Once unlinked, request a bug refresh with <code>/jira refresh</code>.

[Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) has not been moved to the MODIFIED state.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:   "External bug on rep that is not in our config is ignored, bug gets set to MODIFIED",
			merged: true,
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{}}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/unreferenced/repo/pull/22",
				Title: "unreferenced/repo#22: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			}},
			prs:           []github.PullRequest{{Number: 22, Merged: false, State: "open"}},
			options:       JiraBranchOptions{StateAfterMerge: &modified}, // no requirements --> always valid
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Status: &jira.Status{Name: "MODIFIED"}}},
			expectedComment: `org/repo#1:@user: All pull requests linked via external trackers have merged:


[Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) has been moved to the MODIFIED state.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name:   "valid bug on merged PR with one external link but no status after merge configured does nothing",
			merged: true,
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123"}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			}},
			prs:           []github.PullRequest{{Number: base.number, Merged: true}},
			options:       JiraBranchOptions{}, // no requirements --> always valid
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123"},
		},
		{
			name:    "valid bug on merged PR with one external link but no referenced bug in the title does nothing",
			merged:  true,
			missing: true,
			issues:  []jira.Issue{{ID: "1", Key: "OCPBUGS-123"}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			}},
			prs:           []github.PullRequest{{Number: base.number, Merged: true}},
			options:       JiraBranchOptions{StateAfterMerge: &modified}, // no requirements --> always valid
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123"},
		},
		{
			name:           "valid bug on merged PR with one external link fails to update bug and comments",
			merged:         true,
			issues:         []jira.Issue{{ID: "1", Key: "OCPBUGS-123"}},
			issueGetErrors: map[string]error{"OCPBUGS-123": errors.New("injected error getting bug")},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			}},
			prs:     []github.PullRequest{{Number: base.number, Merged: true}},
			options: JiraBranchOptions{StateAfterMerge: &modified}, // no requirements --> always valid
			expectedComment: `org/repo#1:@user: An error was encountered searching for bug OCPBUGS-123 on the Jira server at https://my-jira.com. No known errors were detected, please see the full error message for details.

<details><summary>Full error message.</summary>

<code>
injected error getting bug
</code>

</details>

Please contact an administrator to resolve this issue, then request a bug refresh with <code>/jira refresh</code>.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123"},
		},
		{
			name:   "valid bug on merged PR with merged external links but unknown status does not migrate to new state and comments",
			merged: true,
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField: severityCritical,
				},
			}}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			}},
			prs:     []github.PullRequest{{Number: base.number, Merged: true}},
			options: JiraBranchOptions{StateAfterValidation: &updated, StateAfterMerge: &modified}, // no requirements --> always valid
			expectedComment: `org/repo#1:@user: [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) is in an unrecognized state (CLOSED) and will not be moved to the MODIFIED state.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,

			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField: severityCritical,
				},
			}},
		},
		{
			name:   "closed PR removes link and comments",
			merged: false,
			closed: true,
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField: severityCritical,
				},
			}}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			}},
			prs:     []github.PullRequest{{Number: base.number, Merged: false}},
			options: JiraBranchOptions{AddExternalLink: &yes},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123). The bug has been updated to no longer refer to the pull request using the external bug tracker.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField: severityCritical,
				},
			}},
			expectedRemovedRemoteLinks: []jira.RemoteLink{{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			},
		},
		{
			name:   "closed PR without a link does nothing",
			merged: false,
			closed: true,
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField: severityCritical,
				},
			}}},
			prs:     []github.PullRequest{{Number: base.number, Merged: false}},
			options: JiraBranchOptions{AddExternalLink: &yes},
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField: severityCritical,
				},
			}},
		},
		{
			name:   "closed PR removes link, changes bug state, and comments",
			merged: false,
			closed: true,
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}}},
				Status: &jira.Status{Name: "POST"},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField: severityCritical,
				},
			}}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			}},
			prs:     []github.PullRequest{{Number: base.number, Merged: false}},
			options: JiraBranchOptions{AddExternalLink: &yes, StateAfterClose: &JiraBugState{Status: "NEW"}},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123). The bug has been updated to no longer refer to the pull request using the external bug tracker. All external bug links have been closed. The bug has been moved to the NEW state.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "NEW"},
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}, {
					Body:       "Bug status changed to NEW as previous linked PR https://github.com/org/repo/pull/1 has been closed",
					Visibility: PrivateVisibility,
				}}},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField: severityCritical,
				},
			}},
			expectedRemovedRemoteLinks: []jira.RemoteLink{{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			},
		},
		{
			name:    "closed PR with missing bug does nothing",
			merged:  false,
			closed:  true,
			missing: true,
			issues:  []jira.Issue{},
			prs:     []github.PullRequest{{Number: base.number, Merged: false}},
			options: JiraBranchOptions{AddExternalLink: &yes, StateAfterClose: &JiraBugState{Status: "NEW"}},
		},
		{
			name:   "closed PR with multiple external links removes link, does not change bug state, and comments",
			merged: false,
			closed: true,
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "POST"},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField: severityCritical,
				},
			}}},
			remoteLinks: map[string][]jira.RemoteLink{"OCPBUGS-123": {{
				ID: 1,
				Object: &jira.RemoteLinkObject{
					URL:   "https://github.com/org/repo/pull/1",
					Title: "org/repo#1: OCPBUGS-123: fixed it!",
					Icon: &jira.RemoteLinkIcon{
						Url16x16: "https://github.com/favicon.ico",
						Title:    "GitHub",
					},
				},
			}, {
				ID: 2,
				Object: &jira.RemoteLinkObject{
					URL:   "https://github.com/org/repo/issues/42",
					Title: "org/repo#42: OCPBUGS-123: fixed it!",
					Icon: &jira.RemoteLinkIcon{
						Url16x16: "https://github.com/favicon.ico",
						Title:    "GitHub",
					},
				}},
			}},
			prs:     []github.PullRequest{{Number: base.number, Merged: false}},
			options: JiraBranchOptions{AddExternalLink: &yes, StateAfterClose: &JiraBugState{Status: "NEW"}},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123). The bug has been updated to no longer refer to the pull request using the external bug tracker.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "POST"},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField: severityCritical,
				},
			}},
			expectedRemovedRemoteLinks: []jira.RemoteLink{{ID: 1, Object: &jira.RemoteLinkObject{
				URL:   "https://github.com/org/repo/pull/1",
				Title: "org/repo#1: OCPBUGS-123: fixed it!",
				Icon: &jira.RemoteLinkIcon{
					Url16x16: "https://github.com/favicon.ico",
					Title:    "GitHub",
				},
			}},
			},
		},
		{
			name: "Cherrypick PR results in cloned bug creation",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}}},
				Project: jira.Project{
					Name: "OCPBUGS",
				},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      severityCritical,
					helpers.TargetVersionField: &v2,
				},
			}}},
			prs:                 []github.PullRequest{{Number: base.number, Body: base.body, Title: base.title}, {Number: 2, Body: "This is an automated cherry-pick of #1.\n\n/assign user", Title: "[v1] " + base.title}},
			title:               "[v1] " + base.title,
			cherrypick:          true,
			cherryPickFromPRNum: 1,
			options:             JiraBranchOptions{TargetVersion: &v1Str},
			expectedComment: `org/repo#1:@user: [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) has been cloned as [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124). Retitling PR to link against new bug.
/retitle [v1] OCPBUGS-124: fixed it!

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "2", Key: "OCPBUGS-124", Fields: &jira.IssueFields{
				Description: "This is a clone of issue OCPBUGS-123. The following is the description of the original issue: \n---\n",
				Status:      &jira.Status{Name: "CLOSED"},
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}}},
				Project: jira.Project{
					Name: "OCPBUGS",
				},
				IssueLinks: []*jira.IssueLink{&cloneLinkTo123JustID, &blocksLinkTo123JustID},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      map[string]interface{}{"Value": `<img alt="" src="/images/icons/priorities/critical.svg" width="16" height="16"> Critical`},
					helpers.TargetVersionField: []interface{}{map[string]interface{}{"name": v1Str}},
				},
			}},
		},
		{
			name: "Cherrypick comment results in cloned bug creation",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}}},
				Project: jira.Project{
					Name: "OCPBUGS",
				},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      severityCritical,
					helpers.TargetVersionField: &v2,
				},
			}}},
			prs: []github.PullRequest{{Number: 2, Body: "This is a manually created cherrypick of #1.\n\n/assign user", Title: "[v1] " + base.title}},
			overrideEvent: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 2, key: "OCPBUGS-123", body: "/jira cherrypick OCPBUGS-123", title: "fixed it!", htmlUrl: "https://github.com/org/repo/pull/1", login: "user", cherrypick: true, cherrypickCmd: true, missing: true,
			},
			cherrypick: true,
			missing:    true,
			options:    JiraBranchOptions{TargetVersion: &v1Str},
			expectedComment: `org/repo#2:@user: [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) has been cloned as [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124). Retitling PR to link against new bug.
/retitle OCPBUGS-124: fixed it!

<details>

In response to [this](https://github.com/org/repo/pull/1):

>/jira cherrypick OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "2", Key: "OCPBUGS-124", Fields: &jira.IssueFields{
				Description: "This is a clone of issue OCPBUGS-123. The following is the description of the original issue: \n---\n",
				Status:      &jira.Status{Name: "CLOSED"},
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}}},
				Project: jira.Project{
					Name: "OCPBUGS",
				},
				IssueLinks: []*jira.IssueLink{&cloneLinkTo123JustID, &blocksLinkTo123JustID},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      map[string]interface{}{"Value": `<img alt="" src="/images/icons/priorities/critical.svg" width="16" height="16"> Critical`},
					helpers.TargetVersionField: []interface{}{map[string]interface{}{"name": v1Str}},
				},
			}},
		},
		{
			name: "parent PR of cherrypick not existing results in error",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}}},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      severityCritical,
					helpers.TargetVersionField: &v2,
				},
			}}},
			prs:                 []github.PullRequest{{Number: 2, Body: "This is an automated cherry-pick of #1.\n\n/assign user", Title: "[v1] " + base.title}},
			title:               "[v1] " + base.title,
			cherrypick:          true,
			cherryPickFromPRNum: 1,
			options:             JiraBranchOptions{TargetVersion: &v1Str},
			expectedComment: `org/repo#1:@user: Error creating a cherry-pick bug in Jira: failed to check the state of cherrypicked pull request at https://github.com/org/repo/pull/1: pull request number 1 does not exist.
Please contact an administrator to resolve this issue, then request a bug refresh with <code>/jira refresh</code>.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name: "failure to obtain parent bug for cherrypick results in error",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}}},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      severityCritical,
					helpers.TargetVersionField: &v2,
				},
			}}},
			issueGetErrors:      map[string]error{"OCPBUGS-123": errors.New("injected error getting bug")},
			prs:                 []github.PullRequest{{Number: base.number, Body: base.body, Title: base.title}, {Number: 2, Body: "This is an automated cherry-pick of #1.\n\n/assign user", Title: "[v1] " + base.title}},
			title:               "[v1] " + base.title,
			cherrypick:          true,
			cherryPickFromPRNum: 1,
			options:             JiraBranchOptions{TargetVersion: &v1Str},
			expectedComment: `org/repo#1:@user: An error was encountered searching for bug OCPBUGS-123 on the Jira server at https://my-jira.com. No known errors were detected, please see the full error message for details.

<details><summary>Full error message.</summary>

<code>
injected error getting bug
</code>

</details>

Please contact an administrator to resolve this issue, then request a bug refresh with <code>/jira refresh</code>.

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		}, {
			name: "failure to update bug retitles the PR and prints a warning in the comment",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}}},
				Project: jira.Project{
					Name: "OCPBUGS",
				},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      severityCritical,
					helpers.TargetVersionField: &v2,
				},
			}}},
			issueUpdateErrors:   map[string]error{"OCPBUGS-124": errors.New("injected error updating bug OCPBUGS-124")},
			prs:                 []github.PullRequest{{Number: base.number, Body: base.body, Title: base.title}, {Number: 2, Body: "This is an automated cherry-pick of #1.\n\n/assign user", Title: "[v1] " + base.title}},
			title:               "[v1] " + base.title,
			cherrypick:          true,
			cherryPickFromPRNum: 1,
			options:             JiraBranchOptions{TargetVersion: &v1Str},
			expectedComment: `org/repo#1:@user: [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) has been cloned as [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124). Retitling PR to link against new bug.
/retitle [v1] OCPBUGS-124: fixed it!

WARNING: Failed to update the target version for the clone. Please update the target version manually. Full error below:
<details><summary>Full error message.</summary>

<code>
injected error updating bug OCPBUGS-124
</code>

</details>

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		}, {
			name: "If bug clone with correct target version already exists, just retitle PR",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				IssueLinks: []*jira.IssueLink{&cloneLinkTo124, &blocksLinkTo124},
				Status:     &jira.Status{Name: "CLOSED"},
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}}},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      severityCritical,
					helpers.TargetVersionField: &v2,
				},
			}}, {ID: "2", Key: "OCPBUGS-124", Fields: &jira.IssueFields{
				IssueLinks: []*jira.IssueLink{&cloneLinkTo123, &blocksLinkTo123},
				Status:     &jira.Status{Name: "NEW"},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      severityCritical,
					helpers.TargetVersionField: &v1,
				},
			}},
			},
			prs:                 []github.PullRequest{{Number: base.number, Body: base.body, Title: base.title}, {Number: 2, Body: "This is an automated cherry-pick of #1.\n\n/assign user", Title: "[v1] " + base.title}},
			title:               "[v1] " + base.title,
			cherrypick:          true,
			cherryPickFromPRNum: 1,
			options:             JiraBranchOptions{TargetVersion: &v1Str},
			expectedComment: `org/repo#1:@user: Detected clone of [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) with correct target version. Retitling PR to link to clone:
/retitle [v1] OCPBUGS-124: fixed it!

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		}, {
			name: "Clone for different version does not block creation of new clone",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}}},
				Project: jira.Project{
					Name: "OCPBUGS",
				},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      severityCritical,
					helpers.TargetVersionField: &v2,
				},
			}}, {ID: "2", Key: "OCPBUGS-124", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "NEW"},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      severityCritical,
					helpers.TargetVersionField: &v3,
				},
			}},
			},
			prs:                 []github.PullRequest{{Number: base.number, Body: base.body, Title: base.title}, {Number: 2, Body: "This is an automated cherry-pick of #1.\n\n/assign user", Title: "[v1] " + base.title}},
			title:               "[v1] " + base.title,
			cherrypick:          true,
			cherryPickFromPRNum: 1,
			options:             JiraBranchOptions{TargetVersion: &v1Str},
			expectedComment: `org/repo#1:@user: [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) has been cloned as [Jira Issue OCPBUGS-125](https://my-jira.com/browse/OCPBUGS-125). Retitling PR to link against new bug.
/retitle [v1] OCPBUGS-125: fixed it!

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "3", Key: "OCPBUGS-125", Fields: &jira.IssueFields{
				Description: "This is a clone of issue OCPBUGS-123. The following is the description of the original issue: \n---\n",
				Status:      &jira.Status{Name: "CLOSED"}, // during a clone on a real jira server, this field would get unset/reset; the fake client copies
				IssueLinks:  []*jira.IssueLink{&cloneLinkTo123JustID, &blocksLinkTo123JustID},
				Comments: &jira.Comments{Comments: []*jira.Comment{{
					Body: "This is a bug",
				}}},
				Project: jira.Project{
					Name: "OCPBUGS",
				},
				Unknowns: tcontainer.MarshalMap{
					helpers.SeverityField:      map[string]interface{}{"Value": `<img alt="" src="/images/icons/priorities/critical.svg" width="16" height="16"> Critical`},
					helpers.TargetVersionField: []interface{}{map[string]interface{}{"name": v1Str}},
				},
			}},
		}, {
			name:    "Bug with non-allowed security level is ignored",
			issues:  []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Unknowns: tcontainer.MarshalMap{"security": jiraclient.SecurityLevel{Name: "security"}}}}},
			options: JiraBranchOptions{AllowedSecurityLevels: []string{"internal"}},
			prs:     []github.PullRequest{{Number: base.number, Body: base.body, Title: base.title}},
			// there should be no comment returned in this test case
		}, {
			name:           "Bug with security level on repo with no allowed security levels results in comment on /jira refresh",
			issues:         []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Unknowns: tcontainer.MarshalMap{"security": jiraclient.SecurityLevel{Name: "security"}}}}},
			prs:            []github.PullRequest{{Number: base.number, Body: base.body, Title: base.title}},
			refresh:        true,
			body:           "/jira refresh",
			expectedLabels: []string{labels.JiraValidRef, labels.JiraValidBug, labels.BugzillaValidBug},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123), which is valid.

<details><summary>No validations were run on this bug</summary></details>

<details>

In response to [this](https://github.com/org/repo/pull/1):

>/jira refresh


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		}, {
			name:    "Bug with non-allowed security level results in comment on /jira refresh",
			issues:  []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Unknowns: tcontainer.MarshalMap{"security": jiraclient.SecurityLevel{Name: "security"}}}}},
			prs:     []github.PullRequest{{Number: base.number, Body: base.body, Title: base.title}},
			refresh: true,
			body:    "/jira refresh",
			options: JiraBranchOptions{AllowedSecurityLevels: []string{"internal"}},
			expectedComment: `org/repo#1:@user: [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) is in a security level that is not in the allowed security levels for this repo.
Allowed security levels for this repo are:
- internal

<details>

In response to [this](https://github.com/org/repo/pull/1):

>/jira refresh


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		}, {
			name:    "Bug with non-allowed security level results in comment on PR creation",
			issues:  []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Unknowns: tcontainer.MarshalMap{"security": jiraclient.SecurityLevel{Name: "security"}}}}},
			prs:     []github.PullRequest{{Number: base.number, Body: base.body, Title: base.title}},
			opened:  true,
			options: JiraBranchOptions{AllowedSecurityLevels: []string{"internal"}},
			expectedComment: `org/repo#1:@user: [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) is in a security level that is not in the allowed security levels for this repo.
Allowed security levels for this repo are:
- internal

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		}, {
			name: "Bug with allowed group is properly handled",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{Unknowns: tcontainer.MarshalMap{
				"security":            jiraclient.SecurityLevel{Name: "security"},
				helpers.SeverityField: severityModerate,
			}}}},
			options:        JiraBranchOptions{StateAfterValidation: &updated, AllowedSecurityLevels: []string{"security"}},
			labels:         []string{labels.JiraInvalidBug},
			expectedLabels: []string{labels.JiraValidRef, labels.JiraValidBug, labels.BugzillaValidBug, labels.SeverityModerate},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123), which is valid. The bug has been moved to the UPDATED state.

<details><summary>No validations were run on this bug</summary></details>

<details>

In response to [this](https://github.com/org/repo/pull/1):

>This PR fixes OCPBUGS-123


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Unknowns: tcontainer.MarshalMap{
					"security":            jiraclient.SecurityLevel{Name: "security"},
					helpers.SeverityField: severityModerate,
				}, Status: &jira.Status{Name: "UPDATED"},
			}},
		}, {
			name: "Bug with dependent bug not in OCPBUGS is invalid",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGSM-123", Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "VERIFIED"},
				IssueLinks: []*jira.IssueLink{&cloneLinkTo124, &blocksLinkTo124},
				Unknowns: tcontainer.MarshalMap{
					helpers.TargetVersionField: &v2,
				},
			},
			}, {ID: "2", Key: "OCPBUGS-124", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "MODIFIED"},
				IssueLinks: []*jira.IssueLink{{
					Type: jira.IssueLinkType{
						Name:    "Blocks",
						Inward:  "is blocked by",
						Outward: "blocks",
					},
					InwardIssue: &jira.Issue{ID: "1", Key: "OCPBUGSM-123"},
				}},
				Unknowns: tcontainer.MarshalMap{
					helpers.TargetVersionField: &v1,
				},
			}}},
			overrideEvent: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 2, key: "OCPBUGS-124", isBug: true, body: "This PR fixes OCPBUGS-124", title: "OCPBUGS-124: fixed it!", htmlUrl: "https://github.com/org/repo/pull/2", login: "user",
			},
			existingIssueLinks: []*jira.IssueLink{{
				Type: jira.IssueLinkType{
					Name:    "Blocks",
					Inward:  "is blocked by",
					Outward: "blocks",
				},
				InwardIssue:  &jira.Issue{ID: "1", Key: "OCPBUGSM-123"},
				OutwardIssue: &jira.Issue{ID: "2", Key: "OCPBUGS-124"},
			}},
			options:        JiraBranchOptions{IsOpen: &yes, TargetVersion: &v1Str, DependentBugStates: &verified, DependentBugTargetVersions: &[]string{v2Str}},
			labels:         []string{},
			expectedLabels: []string{labels.JiraValidRef, labels.JiraInvalidBug},
			expectedComment: `org/repo#2:@user: This pull request references [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124), which is invalid:
 - bug is open, matching expected state (open)
 - bug target version (v1) matches configured target version for branch (v1)
 - bug has dependents
 - dependent bug OCPBUGSM-123 is not in the required ` + "`OCPBUGS`" + ` project

All dependent bugs must be part of the OCPBUGS project. If you are backporting a fix that was originally tracked in Bugzilla, follow these steps to handle the backport:
1. Create a new bug in the OCPBUGS Jira project to match the original bugzilla bug. The important fields that should match are the title, description, target version, and status.
2. Use the Jira UI to clone the Jira bug, then in the clone bug:
  a. Set the target version to the release you are cherrypicking to.
  b. Add an issue link “is blocked by”, which links to the original jira bug
3. Use the cherrypick github command to create the cherrypicked PR. Once that new PR is created, retitle the PR and replace the BUG XXX: with OCPBUGS-XXX: to match the new Jira story.

Note that the mirrored bug in OCPBUGSM should not be involved in this process at all.

Comment <code>/jira refresh</code> to re-evaluate validity if changes to the Jira bug are made, or edit the title of this pull request to link to a different bug.

<details>

In response to [this](https://github.com/org/repo/pull/2):

>This PR fixes OCPBUGS-124


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		}, {
			name: "Cherrypick PR on Bugzilla cherrypick creates new issues with BZ link",
			bugs: []bugzilla.Bug{{
				Summary:       "This is a title",
				TargetRelease: []string{v2Str},
				ID:            1,
				Component:     []string{"Installer"},
			}},
			bugComments: map[int][]bugzilla.Comment{
				1: {{
					Text: "This is a description",
				}},
			},
			bugSubComponents: map[int]map[string][]string{
				1: {
					"Installer": {
						"openshift-ansible",
					},
				},
			},
			overrideEvent: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 2, key: "", body: "This is an automated cherry-pick of #1.\n\n/assign user", title: "[v1] Bug 1: fixed it!", htmlUrl: "https://github.com/org/repo/pull/2", login: "user",
			},
			prs:                 []github.PullRequest{{Number: base.number, Body: base.body, Title: "Bug 1: fixed it!"}, {Number: 2, Body: "This is an automated cherry-pick of #1.\n\n/assign user", Title: "[v1] " + "Bug 1: fixed it!"}},
			title:               "[v1] Bug 1: fixed it!",
			cherrypick:          true,
			cherryPickFromPRNum: 1,
			options:             JiraBranchOptions{TargetVersion: &v1Str},
			expectedComment: `org/repo#2:@user: [Bugzilla Bug 1](www.bugzilla/show_bug.cgi?id=1) has been cloned as [Jira Issue -1](https://my-jira.com/browse/-1). Retitling PR to link against new bug.
/retitle [v1] -1: fixed it!

<details>

In response to [this](https://github.com/org/repo/pull/2):

>This is an automated cherry-pick of #1.
>
>/assign user


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "-1", Fields: &jira.IssueFields{
				Summary:     "This is a title",
				Description: "This bug is a backport clone of [Bugzilla Bug 1](www.bugzilla/show_bug.cgi?id=1). The following is the description of the original bug:\n---\nThis is a description",
				Project: jira.Project{
					Key: "OCPBUGS",
				},
				Type: jira.IssueType{
					Name: "Bug",
				},
				AffectsVersions: []*jira.AffectsVersion{{Name: v1Str}},
				Components:      []*jira.Component{{Name: "Installer / openshift-ansible"}},
				Unknowns: tcontainer.MarshalMap{
					helpers.TargetVersionField:   v1,
					helpers.BlockedByBugzillaBug: "www.bugzilla/show_bug.cgi?id=1",
				},
			}},
		}, {
			name: "Cherrypick PR on Bugzilla cherrypick for CVE creates new bug with correct labels",
			bugs: []bugzilla.Bug{{
				Summary:       "This is a title",
				TargetRelease: []string{v2Str},
				ID:            1,
				Component:     []string{"Installer"},
				Keywords:      []string{"Security", "SecurityTracking"},
				Whiteboard:    "component:test-component",
				Blocks:        []int{2},
			}, {
				ID:    2,
				Alias: []string{"CVE-2022-12345"},
			}},
			bugComments: map[int][]bugzilla.Comment{
				1: {{
					Text: "This is a description",
				}},
			},
			bugSubComponents: map[int]map[string][]string{
				1: {
					"Installer": {
						"openshift-ansible",
					},
				},
			},
			overrideEvent: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 2, key: "", body: "This is an automated cherry-pick of #1.\n\n/assign user", title: "[v1] Bug 1: fixed it!", htmlUrl: "https://github.com/org/repo/pull/2", login: "user",
			},
			prs:                 []github.PullRequest{{Number: base.number, Body: base.body, Title: "Bug 1: fixed it!"}, {Number: 2, Body: "This is an automated cherry-pick of #1.\n\n/assign user", Title: "[v1] " + "Bug 1: fixed it!"}},
			title:               "[v1] Bug 1: fixed it!",
			cherrypick:          true,
			cherryPickFromPRNum: 1,
			options:             JiraBranchOptions{TargetVersion: &v1Str},
			expectedComment: `org/repo#2:@user: [Bugzilla Bug 1](www.bugzilla/show_bug.cgi?id=1) has been cloned as [Jira Issue -1](https://my-jira.com/browse/-1). Retitling PR to link against new bug.
/retitle [v1] -1: fixed it!

<details>

In response to [this](https://github.com/org/repo/pull/2):

>This is an automated cherry-pick of #1.
>
>/assign user


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedIssue: &jira.Issue{ID: "1", Key: "-1", Fields: &jira.IssueFields{
				Summary:     "This is a title",
				Description: "This bug is a backport clone of [Bugzilla Bug 1](www.bugzilla/show_bug.cgi?id=1). The following is the description of the original bug:\n---\nThis is a description",
				Project: jira.Project{
					Key: "OCPBUGS",
				},
				Type: jira.IssueType{
					Name: "Bug",
				},
				AffectsVersions: []*jira.AffectsVersion{{Name: v1Str}},
				Components:      []*jira.Component{{Name: "Installer / openshift-ansible"}},
				Unknowns: tcontainer.MarshalMap{
					helpers.TargetVersionField:   v1,
					helpers.BlockedByBugzillaBug: "www.bugzilla/show_bug.cgi?id=1",
				},
				Labels: []string{"Security", "SecurityTracking", "component:test-component", "CVE-2022-12345", "flaw:bz#2"},
			}},
		}, {
			name: "Existing issue has labels updated on refresh",
			issues: []jira.Issue{{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "MODIFIED"},
				Unknowns: tcontainer.MarshalMap{
					helpers.BlockedByBugzillaBug: "www.bugzilla/show_bug.cgi?id=1",
					helpers.TargetVersionField:   v1,
				}}},
			},
			refresh: true,
			body:    "/jira refresh",
			bugs: []bugzilla.Bug{{
				ID:            1,
				TargetRelease: []string{v2Str},
				Status:        "VERIFIED",
				Keywords:      []string{"Security", "SecurityTracking"},
				Whiteboard:    "component:test-component",
				Blocks:        []int{2},
			}, {
				ID:    2,
				Alias: []string{"CVE-2022-12345"},
			}},
			title:   "Bug 1: fixed it!",
			options: JiraBranchOptions{ValidateByDefault: &yes, IsOpen: &yes, TargetVersion: &v1Str, DependentBugStates: &verified, DependentBugTargetVersions: &[]string{v2Str}},
			expectedComment: `org/repo#1:@user: This pull request references [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123), which is valid.

<details><summary>5 validation(s) were run on this bug</summary>

* bug is open, matching expected state (open)
* bug target version (v1) matches configured target version for branch (v1)
* dependent bug [Bugzilla Bug 1](www.bugzilla/show_bug.cgi?id=1) is in the state VERIFIED, which is one of the valid states (VERIFIED)
* dependent [Bugzilla Bug 1](www.bugzilla/show_bug.cgi?id=1) targets the "v2" version, which is one of the valid target versions: v2
* bug has dependents</details>

<details>

In response to [this](https://github.com/org/repo/pull/1):

>/jira refresh


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
			expectedLabels: []string{labels.JiraValidRef, labels.JiraValidBug, labels.BugzillaValidBug},
			expectedIssue: &jira.Issue{ID: "1", Key: "OCPBUGS-123", Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "MODIFIED"},
				Unknowns: tcontainer.MarshalMap{
					helpers.TargetVersionField:   []interface{}{map[string]interface{}{"name": string("v1")}},
					helpers.BlockedByBugzillaBug: "www.bugzilla/show_bug.cgi?id=1",
				},
				Labels: []string{"CVE-2022-12345", "Security", "SecurityTracking", "component:test-component", "flaw:bz#2"},
			}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var ptrIssues []*jira.Issue
			for index := range tc.issues {
				ptrIssues = append(ptrIssues, &tc.issues[index])
			}
			jiraClient := &fakejira.FakeClient{
				Issues:           ptrIssues,
				ExistingLinks:    tc.remoteLinks,
				GetIssueError:    tc.issueGetErrors,
				CreateIssueError: tc.issueCreateErrors,
				UpdateIssueError: tc.issueUpdateErrors,
				Transitions:      jiraTransitions,
			}
			var testEvent event // copy so parallel tests don't collide
			if tc.overrideEvent != nil {
				testEvent = *tc.overrideEvent
			} else {
				testEvent = *base // copy so parallel tests don't collide
			}
			testEvent.refresh = tc.refresh
			testEvent.missing = tc.missing
			testEvent.merged = tc.merged
			testEvent.closed = tc.closed || tc.merged
			testEvent.opened = tc.opened
			testEvent.isBug = !tc.nonBug
			testEvent.cherrypick = tc.cherrypick
			testEvent.cherrypickFromPRNum = tc.cherryPickFromPRNum
			if tc.body != "" {
				testEvent.body = tc.body
			}
			if tc.title != "" {
				testEvent.title = tc.title
			}
			if tc.key != "" {
				testEvent.key = tc.key
			}

			gc := fakegithub.NewFakeClient()
			gc.IssueLabelsExisting = []string{}
			gc.IssueComments = map[int][]github.IssueComment{}
			gc.PullRequests = map[int]*github.PullRequest{}
			gc.WasLabelAddedByHumanVal = tc.humanLabelled
			for _, label := range tc.labels {
				gc.IssueLabelsExisting = append(gc.IssueLabelsExisting, fmt.Sprintf("%s/%s#%d:%s", testEvent.org, testEvent.repo, testEvent.number, label))
			}
			for _, pr := range tc.prs {
				pr := pr
				gc.PullRequests[pr.Number] = &pr
			}
			// the test-infra fake github client does not implement a Query function; we don't test the query functionality here, so we can just wrap the test-infra
			// client with a custom one that has an empty Query function
			// TODO: implement a basic fake query function in test-infra fakegithub library and start unit testing the query path
			fakeClient := fakeGHClient{gc}
			bc := bugzilla.Fake{
				EndpointString: "www.bugzilla",
				Bugs:           map[int]bugzilla.Bug{},
				BugComments:    tc.bugComments,
				SubComponents:  map[int]map[string][]string{},
			}
			for _, bug := range tc.bugs {
				bc.Bugs[bug.ID] = bug
			}
			for id, subComponent := range tc.bugSubComponents {
				bc.SubComponents[id] = subComponent
			}
			if err := handle(jiraClient, fakeClient, &bc, tc.options, logrus.WithField("testCase", tc.name), testEvent, sets.NewString("org/repo")); err != nil {
				t.Fatalf("handle failed: %v", err)
			}

			if diff := cmp.Diff(jiraClient.NewLinks, tc.expectedNewRemoteLinks); diff != "" {
				t.Errorf("new links differs from expected new links: %s", diff)
			}

			if diff := cmp.Diff(jiraClient.RemovedLinks, tc.expectedRemovedRemoteLinks); diff != "" {
				t.Errorf("removed links differs from expected removed links: %s", diff)
			}

			if diff := cmp.Diff(gc.IssueCommentsEdited, tc.expectedCommentUpdates); diff != "" {
				t.Errorf("comment updates differ from expected: %s", diff)
			}

			checkComments(gc, tc.name, tc.expectedComment, t)

			expected := sets.NewString()
			for _, label := range tc.expectedLabels {
				expected.Insert(fmt.Sprintf("%s/%s#%d:%s", testEvent.org, testEvent.repo, testEvent.number, label))
			}

			actual := sets.NewString(gc.IssueLabelsExisting...)
			actual.Insert(gc.IssueLabelsAdded...)
			actual.Delete(gc.IssueLabelsRemoved...)

			if missing := expected.Difference(actual); missing.Len() > 0 {
				t.Errorf("%s: missing expected labels: %v", tc.name, missing.List())
			}
			if extra := actual.Difference(expected); extra.Len() > 0 {
				t.Errorf("%s: unexpected labels: %v", tc.name, extra.List())
			}

			// reset errors on client for verification
			jiraClient.CreateIssueError = nil
			jiraClient.GetIssueError = nil
			if tc.expectedIssue != nil {
				actual, err := jiraClient.GetIssue(tc.expectedIssue.ID)
				if err != nil {
					t.Errorf("%s: failed to get expected bug during test: %v", tc.name, err)
				}
				if !reflect.DeepEqual(actual, tc.expectedIssue) {
					t.Errorf("%s: got incorrect bug after update: %s", tc.name, cmp.Diff(actual, tc.expectedIssue, allowEventAndDate))
				}
			}
		})
	}
}

func checkComments(client *fakegithub.FakeClient, name, expectedComment string, t *testing.T) {
	wantedComments := 0
	if expectedComment != "" {
		wantedComments = 1
	}
	if len(client.IssueCommentsAdded) != wantedComments {
		t.Errorf("%s: wanted %d comment, got %d: %v", name, wantedComments, len(client.IssueCommentsAdded), client.IssueCommentsAdded)
	}

	if expectedComment != "" && len(client.IssueCommentsAdded) == 1 {
		if expectedComment != client.IssueCommentsAdded[0] {
			t.Errorf("%s: got incorrect comment: %v", name, cmp.Diff(expectedComment, client.IssueCommentsAdded[0]))
		}
	}
}

func TestInsertLinksIntoComment(t *testing.T) {
	t.Parallel()
	const issueName = "ABC-123"
	testCases := []struct {
		name     string
		body     string
		expected string
	}{
		{
			name: "Multiline body starting with issue name",
			body: `ABC-123: Fix problems:
* First problem
* Second problem`,
			expected: `[ABC-123](https://my-jira.com/browse/ABC-123): Fix problems:
* First problem
* Second problem`,
		},
		{
			name: "Multiline body starting with already replaced issue name",
			body: `[ABC-123](https://my-jira.com/browse/ABC-123): Fix problems:
* First problem
* Second problem`,
			expected: `[ABC-123](https://my-jira.com/browse/ABC-123): Fix problems:
* First problem
* Second problem`,
		},
		{
			name: "Multiline body with multiple occurrence in the middle",
			body: `This change:
* Does stuff related to ABC-123
* And even more stuff related to ABC-123
* But also something else`,
			expected: `This change:
* Does stuff related to [ABC-123](https://my-jira.com/browse/ABC-123)
* And even more stuff related to [ABC-123](https://my-jira.com/browse/ABC-123)
* But also something else`,
		},
		{
			name: "Multiline body with multiple occurrence in the middle, some already replaced",
			body: `This change:
* Does stuff related to [ABC-123](https://my-jira.com/browse/ABC-123)
* And even more stuff related to ABC-123
* But also something else`,
			expected: `This change:
* Does stuff related to [ABC-123](https://my-jira.com/browse/ABC-123)
* And even more stuff related to [ABC-123](https://my-jira.com/browse/ABC-123)
* But also something else`,
		},
		{
			name: "Multiline body with issue name at the end",
			body: `This change:
is very important
because of ABC-123`,
			expected: `This change:
is very important
because of [ABC-123](https://my-jira.com/browse/ABC-123)`,
		},
		{
			name: "Multiline body with already replaced issue name at the end",
			body: `This change:
is very important
because of [ABC-123](https://my-jira.com/browse/ABC-123)`,
			expected: `This change:
is very important
because of [ABC-123](https://my-jira.com/browse/ABC-123)`,
		},
		{
			name:     "Pasted links are not replaced, as they are already clickable",
			body:     "https://my-jira.com/browse/ABC-123",
			expected: "https://my-jira.com/browse/ABC-123",
		},
		{
			name: "code section is not replaced",
			body: `This change:
is very important` + "\n```bash\n" +
				`ABC-123` +
				"\n```\n" + `ABC-123
`,
			expected: `This change:
is very important` + "\n```bash\n" +
				`ABC-123` +
				"\n```\n" + `[ABC-123](https://my-jira.com/browse/ABC-123)
`,
		},
		{
			name: "inline code is not replaced",
			body: `This change:
is very important` + "\n``ABC-123`` and `ABC-123` shouldn't be replaced, as well as ``ABC-123: text text``. " +
				`ABC-123 should be replaced.
`,
			expected: `This change:
is very important` + "\n``ABC-123`` and `ABC-123` shouldn't be replaced, as well as ``ABC-123: text text``. " +
				`[ABC-123](https://my-jira.com/browse/ABC-123) should be replaced.
`,
		},
		{
			name:     "Multiline codeblock that is denoted through four leading spaces",
			body:     "I meant to do this test:\r\n\r\n    operator_test.go:1914: failed to read output from pod unique-id-header-test-1: container \"curl\" in pod \"unique-id-header-ABC-123\" is waiting to start: ContainerCreating\r\n\r\n",
			expected: "I meant to do this test:\r\n\r\n    operator_test.go:1914: failed to read output from pod unique-id-header-test-1: container \"curl\" in pod \"unique-id-header-ABC-123\" is waiting to start: ContainerCreating\r\n\r\n",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(insertLinksIntoComment(tc.body, []string{issueName}, fakejira.FakeJiraUrl), tc.expected); diff != "" {
				t.Errorf("actual result differs from expected result: %s", diff)
			}
		})
	}
}

func TestHelpProvider(t *testing.T) {
	rawConfig := `disabled_jira_projects:
- "private-project"
default:
  "*":
    target_version: global-default
  "global-branch":
    is_open: false
    target_version: global-branch-default
orgs:
  my-org:
    default:
      "*":
        is_open: true
        target_version: my-org-default
        state_after_validation:
          status: "PRE"
      "my-org-branch":
        target_version: my-org-branch-default
        state_after_validation:
          status: "POST"
        add_external_link: true
    repos:
      my-repo:
        branches:
          "*":
            is_open: false
            target_version: my-repo-default
            valid_states:
            - status: VALIDATED
          "my-repo-branch":
            target_version: my-repo-branch
            valid_states:
            - status: MODIFIED
            add_external_link: true
            state_after_merge:
              status: MODIFIED
            allowed_security_levels:
            - default
          "branch-that-likes-closed-bugs":
            valid_states:
            - status: VERIFIED
            - status: CLOSED
              resolution: ERRATA
            dependent_bug_states:
            - status: CLOSED
              resolution: ERRATA
            state_after_merge:
              status: CLOSED
              resolution: FIXED
            state_after_validation:
              status: CLOSED
              resolution: VALIDATED`

	var config Config
	if err := yaml.Unmarshal([]byte(rawConfig), &config); err != nil {
		t.Fatalf("couldn't unmarshal config: %v", err)
	}
	enabledRepos := []prowconfig.OrgRepo{
		{Org: "some-org", Repo: "some-repo"},
		{Org: "my-org", Repo: "some-repo"},
		{Org: "my-org", Repo: "my-repo"},
	}
	serv := &server{
		config: func() *Config {
			return &config
		},
	}
	help, err := serv.helpProvider(enabledRepos)
	if err != nil {
		t.Fatalf("unexpected error creating help provider: %v", err)
	}
	// don't check snippet
	help.Snippet = ""

	expected := &pluginhelp.PluginHelp{
		Description: "The jira plugin ensures that pull requests reference a valid Jira bug in their title.",
		Config: map[string]string{
			"some-org/some-repo": `The plugin has the following configuration:<ul>
<li>by default, valid bugs must target the "global-default" version.</li>
<li>on the "global-branch" branch, valid bugs must be closed and target the "global-branch-default" version.</li>
</ul>`,
			"my-org/some-repo": `The plugin has the following configuration:<ul>
<li>by default, valid bugs must be open and target the "my-org-default" version. After being linked to a pull request, bugs will be moved to the PRE state.</li>
<li>on the "my-org-branch" branch, valid bugs must be open and target the "my-org-branch-default" version. After being linked to a pull request, bugs will be moved to the POST state and updated to refer to the pull request using the external bug tracker.</li>
</ul>`,
			"my-org/my-repo": `The plugin has the following configuration:<ul>
<li>by default, valid bugs must be closed, target the "my-repo-default" version, and be in one of the following states: VALIDATED. After being linked to a pull request, bugs will be moved to the PRE state.</li>
<li>on the "branch-that-likes-closed-bugs" branch, valid bugs must be closed, target the "my-repo-default" version, be in one of the following states: VERIFIED, CLOSED (ERRATA), depend on at least one other bug, and have all dependent bugs in one of the following states: CLOSED (ERRATA). After being linked to a pull request, bugs will be moved to the CLOSED (VALIDATED) state and moved to the CLOSED (FIXED) state when all linked pull requests are merged.</li>
<li>on the "my-org-branch" branch, valid bugs must be closed, target the "my-repo-default" version, and be in one of the following states: VALIDATED. After being linked to a pull request, bugs will be moved to the POST state and updated to refer to the pull request using the external bug tracker.</li>
<li>on the "my-repo-branch" branch, valid bugs must be closed, target the "my-repo-branch" version, and be in one of the following states: MODIFIED. After being linked to a pull request, bugs will be moved to the PRE state, updated to refer to the pull request using the external bug tracker, and moved to the MODIFIED state when all linked pull requests are merged.</li>
</ul>`,
		},
		Commands: []pluginhelp.Command{
			{
				Usage:       "/jira refresh",
				Description: "Check Jira for a valid bug referenced in the PR title",
				Featured:    false,
				WhoCanUse:   "Anyone",
				Examples:    []string{"/jira refresh"},
			}, {
				Usage:       "/jira cc-qa",
				Description: "Request PR review from QA contact specified in Jira",
				Featured:    false,
				WhoCanUse:   "Anyone",
				Examples:    []string{"/jira cc-qa"},
			}, {
				Usage:       "/jira cherrypick jiraBugKey",
				Description: "Cherrypick a jira bug and link it to the current PR",
				Featured:    false,
				WhoCanUse:   "Anyone",
				Examples:    []string{"/jira cherrypick OCPBUGS-1234"},
			},
		},
	}

	if actual := help; !reflect.DeepEqual(actual, expected) {
		t.Errorf("resolved incorrect plugin help: %v", cmp.Diff(actual, expected, allowEventAndDate))
	}
}

func TestDigestPR(t *testing.T) {
	yes := true
	var testCases = []struct {
		name              string
		pre               github.PullRequestEvent
		validateByDefault *bool
		expected          *event
		expectedErr       bool
	}{
		{
			name: "unrelated event gets ignored",
			pre: github.PullRequestEvent{
				Action: github.PullRequestFileAdded,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number: 1,
					Title:  "OCPBUGS-123: fixed it!",
					State:  "open",
				},
			},
		},
		{
			name: "unrelated title gets ignored",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number: 1,
					Title:  "fixing a typo",
					State:  "open",
				},
			},
		},
		{
			name: "unrelated title gets handled when validating by default",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number:  1,
					Title:   "fixing a typo",
					State:   "open",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
				},
			},
			validateByDefault: &yes,
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, state: "open", missing: true, opened: true, key: "", title: "fixing a typo", htmlUrl: "http.com", login: "user",
			},
		},
		{
			name: "title referencing bug gets an event",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number:  1,
					Title:   "OCPBUGS-123: fixed it!",
					State:   "open",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
				},
			},
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, state: "open", opened: true, key: "OCPBUGS-123", isBug: true, title: "OCPBUGS-123: fixed it!", htmlUrl: "http.com", login: "user",
			},
		},
		{
			name: "title referencing non-bug jira gets an event",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number:  1,
					Title:   "SOMEJIRA-123: implement feature!",
					State:   "open",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
				},
			},
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, state: "open", opened: true, key: "SOMEJIRA-123", isBug: false, title: "SOMEJIRA-123: implement feature!", htmlUrl: "http.com", login: "user",
			},
		},
		{
			name: "title explicitly referencing no-issue gets an event",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number:  1,
					Title:   "NO-ISSUE: typo fixup",
					State:   "open",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
				},
			},
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, state: "open", opened: true, key: "NO-JIRA", isBug: false, title: "NO-ISSUE: typo fixup", htmlUrl: "http.com", login: "user",
			},
		},
		{
			name: "title referencing no-jira gets an event",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number:  1,
					Title:   "NO-JIRA: typo fixup",
					State:   "open",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
				},
			},
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, state: "open", opened: true, key: "NO-JIRA", isBug: false, title: "NO-JIRA: typo fixup", htmlUrl: "http.com", login: "user",
			},
		},
		{
			name: "title referencing bug gets an event on PR merge",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionClosed,
				PullRequest: github.PullRequest{
					Merged: true,
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number:  1,
					Title:   "OCPBUGS-123: fixed it!",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
				},
			},
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, merged: true, closed: true, key: "OCPBUGS-123", isBug: true, title: "OCPBUGS-123: fixed it!", htmlUrl: "http.com", login: "user",
			},
		},
		{
			name: "title referencing bug gets an event on PR close",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionClosed,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number:  1,
					Title:   "OCPBUGS-123: fixed it!",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
				},
			},
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, merged: false, closed: true, key: "OCPBUGS-123", isBug: true, title: "OCPBUGS-123: fixed it!", htmlUrl: "http.com", login: "user",
			},
		},
		{
			name: "non-jira cherrypick PR sets e.missing to true",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "release-4.4",
					},
					Number:  3,
					Title:   "[release-4.4] fixing a typo",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
					Body: `This is an automated cherry-pick of #2

/assign user`,
				},
			},
			expected: &event{
				org: "org", repo: "repo", baseRef: "release-4.4", number: 3, opened: true, body: "This is an automated cherry-pick of #2\n\n/assign user", title: "[release-4.4] fixing a typo", htmlUrl: "http.com", login: "user", cherrypick: true, cherrypickFromPRNum: 2, missing: true,
			},
		},
		{
			name: "cherrypicked PR gets cherrypick event",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "release-4.4",
					},
					Number:  3,
					Title:   "[release-4.4] OCPBUGS-123: fixed it!",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
					Body: `This is an automated cherry-pick of #2

/assign user`,
				},
			},
			expected: &event{
				org: "org", repo: "repo", baseRef: "release-4.4", number: 3, opened: true, body: "This is an automated cherry-pick of #2\n\n/assign user", title: "[release-4.4] OCPBUGS-123: fixed it!", htmlUrl: "http.com", login: "user", cherrypick: true, cherrypickFromPRNum: 2, key: "OCPBUGS-123", isBug: true,
			},
		},
		{
			name: "edited cherrypicked PR gets normal event",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionEdited,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "release-4.4",
					},
					Number:  3,
					Title:   "[release-4.4] OCPBUGS-123: fixed it!",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
					Body: `This is an automated cherry-pick of #2

/assign user`,
				},
			},
			expected: &event{
				org: "org", repo: "repo", baseRef: "release-4.4", number: 3, key: "OCPBUGS-123", isBug: true, body: "This is an automated cherry-pick of #2\n\n/assign user", title: "[release-4.4] OCPBUGS-123: fixed it!", htmlUrl: "http.com", login: "user",
			},
		},
		{
			name: "title change referencing same bug gets no event",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number:  1,
					Title:   "OCPBUGS-123: fixed it!",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
				},
				Changes: []byte(`{"title":{"from":"OCPBUGS-123: fixed it! (WIP)"}}`),
			},
		},
		{
			name: "title change referencing new bug gets event",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number:  1,
					Title:   "OCPBUGS-123: fixed it!",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
				},
				Changes: []byte(`{"title":{"from":"fixed it! (WIP)"}}`),
			},
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, opened: true, key: "OCPBUGS-123", isBug: true, title: "OCPBUGS-123: fixed it!", htmlUrl: "http.com", login: "user",
			},
		},
		{
			name: "title change dereferencing bug gets event",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number:  1,
					Title:   "fixed it!",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
				},
				Changes: []byte(`{"title":{"from":"OCPBUGS-123: fixed it! (WIP)"}}`),
			},
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, opened: true, missing: true, title: "fixed it!", htmlUrl: "http.com", login: "user",
			},
		},
		{
			name: "title change to no bug with unrelated changes gets no event",
			pre: github.PullRequestEvent{
				Action: github.PullRequestActionOpened,
				PullRequest: github.PullRequest{
					Base: github.PullRequestBranch{
						Repo: github.Repo{
							Owner: github.User{
								Login: "org",
							},
							Name: "repo",
						},
						Ref: "branch",
					},
					Number:  1,
					Title:   "fixed it!",
					HTMLURL: "http.com",
					User: github.User{
						Login: "user",
					},
				},
				Changes: []byte(`{"oops":{"doops":"payload"}}`),
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			event, err := digestPR(logrus.WithField("testCase", testCase.name), testCase.pre, testCase.validateByDefault)
			if err == nil && testCase.expectedErr {
				t.Errorf("%s: expected an error but got none", testCase.name)
			}
			if err != nil && !testCase.expectedErr {
				t.Errorf("%s: expected no error but got one: %v", testCase.name, err)
			}

			if actual, expected := event, testCase.expected; !reflect.DeepEqual(actual, expected) {
				t.Errorf("%s: did not get correct event: %v", testCase.name, cmp.Diff(actual, expected, allowEventAndDate))
			}
		})
	}
}

func TestDigestComment(t *testing.T) {
	var testCases = []struct {
		name            string
		e               github.IssueCommentEvent
		title           string
		merged          bool
		expected        *event
		expectedComment string
		expectedErr     bool
	}{
		{
			name: "unrelated event gets ignored",
			e: github.IssueCommentEvent{
				Action: github.IssueCommentActionDeleted,
				Issue: github.Issue{
					Number:      1,
					PullRequest: &struct{}{},
				},
				Comment: github.IssueComment{
					Body: "/jira refresh",
				},
				Repo: github.Repo{
					Owner: github.User{
						Login: "org",
					},
					Name: "repo",
				},
			},
			title: "OCPBUGS-123: oopsie doopsie",
		},
		{
			name: "unrelated title gets an event saying so",
			e: github.IssueCommentEvent{
				Action: github.IssueCommentActionCreated,
				Issue: github.Issue{
					Number:      1,
					PullRequest: &struct{}{},
				},
				Comment: github.IssueComment{
					Body: "/jira refresh",
					User: github.User{
						Login: "user",
					},
					HTMLURL: "www.com",
				},
				Repo: github.Repo{
					Owner: github.User{
						Login: "org",
					},
					Name: "repo",
				},
			},
			title: "cole, please review this typo fix",
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, missing: true, body: "/jira refresh", htmlUrl: "www.com", login: "user", refresh: true, cc: false,
			},
		},
		{
			name: "comment on issue gets no event but a comment",
			e: github.IssueCommentEvent{
				Action: github.IssueCommentActionCreated,
				Issue: github.Issue{
					Number: 1,
				},
				Comment: github.IssueComment{
					Body: "/jira refresh",
				},
				Repo: github.Repo{
					Owner: github.User{
						Login: "org",
					},
					Name: "repo",
				},
			},
			title: "someone misspelled words in this repo",
			expectedComment: `org/repo#1:@: Jira bug referencing is only supported for Pull Requests, not issues.

<details>

In response to [this]():

>/jira refresh


Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%20issue:) repository.
</details>`,
		},
		{
			name: "title referencing bug gets an event",
			e: github.IssueCommentEvent{
				Action: github.IssueCommentActionCreated,
				Issue: github.Issue{
					Number:      1,
					PullRequest: &struct{}{},
				},
				Comment: github.IssueComment{
					Body: "/jira refresh",
					User: github.User{
						Login: "user",
					},
					HTMLURL: "www.com",
				},
				Repo: github.Repo{
					Owner: github.User{
						Login: "org",
					},
					Name: "repo",
				},
			},
			title: "OCPBUGS-123: oopsie doopsie",
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, key: "OCPBUGS-123", isBug: true, body: "/jira refresh", htmlUrl: "www.com", login: "user", refresh: true, cc: false,
			},
		},
		{
			name: "title referencing jira gets an event",
			e: github.IssueCommentEvent{
				Action: github.IssueCommentActionCreated,
				Issue: github.Issue{
					Number:      1,
					PullRequest: &struct{}{},
				},
				Comment: github.IssueComment{
					Body: "/jira refresh",
					User: github.User{
						Login: "user",
					},
					HTMLURL: "www.com",
				},
				Repo: github.Repo{
					Owner: github.User{
						Login: "org",
					},
					Name: "repo",
				},
			},
			title: "SOMEJIRA-123: oopsie doopsie",
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, key: "SOMEJIRA-123", isBug: false, body: "/jira refresh", htmlUrl: "www.com", login: "user", refresh: true, cc: false,
			},
		},
		{
			name: "title referencing no-jira gets an event",
			e: github.IssueCommentEvent{
				Action: github.IssueCommentActionCreated,
				Issue: github.Issue{
					Number:      1,
					PullRequest: &struct{}{},
				},
				Comment: github.IssueComment{
					Body: "/jira refresh",
					User: github.User{
						Login: "user",
					},
					HTMLURL: "www.com",
				},
				Repo: github.Repo{
					Owner: github.User{
						Login: "org",
					},
					Name: "repo",
				},
			},
			title: "NO-JIRA: oopsie doopsie",
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, key: "NO-JIRA", isBug: false, body: "/jira refresh", htmlUrl: "www.com", login: "user", refresh: true, cc: false,
			},
		},
		{
			name: "title referencing no-issue gets an event",
			e: github.IssueCommentEvent{
				Action: github.IssueCommentActionCreated,
				Issue: github.Issue{
					Number:      1,
					PullRequest: &struct{}{},
				},
				Comment: github.IssueComment{
					Body: "/jira refresh",
					User: github.User{
						Login: "user",
					},
					HTMLURL: "www.com",
				},
				Repo: github.Repo{
					Owner: github.User{
						Login: "org",
					},
					Name: "repo",
				},
			},
			title: "NO-ISSUE: oopsie doopsie",
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, key: "NO-JIRA", isBug: false, body: "/jira refresh", htmlUrl: "www.com", login: "user", refresh: true, cc: false,
			},
		},
		{
			name: "title referencing bug in a merged PR gets an event",
			e: github.IssueCommentEvent{
				Action: github.IssueCommentActionCreated,
				Issue: github.Issue{
					Number:      1,
					PullRequest: &struct{}{},
				},
				Comment: github.IssueComment{
					Body: "/jira refresh",
					User: github.User{
						Login: "user",
					},
					HTMLURL: "www.com",
				},
				Repo: github.Repo{
					Owner: github.User{
						Login: "org",
					},
					Name: "repo",
				},
			},
			title:  "OCPBUGS-123: oopsie doopsie",
			merged: true,
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, key: "OCPBUGS-123", isBug: true, merged: true, body: "/jira refresh", htmlUrl: "www.com", login: "user", refresh: true, cc: false,
			},
		},
		{
			name: "cc-qa comment event has cc bool set to true",
			e: github.IssueCommentEvent{
				Action: github.IssueCommentActionCreated,
				Issue: github.Issue{
					Number:      1,
					PullRequest: &struct{}{},
				},
				Comment: github.IssueComment{
					Body: "/jira cc-qa",
					User: github.User{
						Login: "user",
					},
					HTMLURL: "www.com",
				},
				Repo: github.Repo{
					Owner: github.User{
						Login: "org",
					},
					Name: "repo",
				},
			},
			title: "OCPBUGS-123: oopsie doopsie",
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, key: "OCPBUGS-123", isBug: true, body: "/jira cc-qa", htmlUrl: "www.com", login: "user", cc: true,
			},
		},
		{
			name: "cherrypick comment event has cherrypick bools set to true and correct bug key set",
			e: github.IssueCommentEvent{
				Action: github.IssueCommentActionCreated,
				Issue: github.Issue{
					Number:      1,
					PullRequest: &struct{}{},
				},
				Comment: github.IssueComment{
					Body: "/jira cherrypick OCPBUGS-1234",
					User: github.User{
						Login: "user",
					},
					HTMLURL: "www.com",
				},
				Repo: github.Repo{
					Owner: github.User{
						Login: "org",
					},
					Name: "repo",
				},
			},
			title: "oopsie doopsie",
			expected: &event{
				org: "org", repo: "repo", baseRef: "branch", number: 1, key: "OCPBUGS-1234", body: "/jira cherrypick OCPBUGS-1234", htmlUrl: "www.com", login: "user", cherrypickCmd: true, missing: true, cherrypick: true,
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			client := fakegithub.NewFakeClient()
			client.PullRequests = map[int]*github.PullRequest{
				1: {Base: github.PullRequestBranch{Ref: "branch"}, Title: testCase.title, Merged: testCase.merged},
			}
			fakeClient := fakeGHClient{client}
			event, err := digestComment(fakeClient, logrus.WithField("testCase", testCase.name), testCase.e)
			if err == nil && testCase.expectedErr {
				t.Errorf("%s: expected an error but got none", testCase.name)
			}
			if err != nil && !testCase.expectedErr {
				t.Errorf("%s: expected no error but got one: %v", testCase.name, err)
			}

			if actual, expected := event, testCase.expected; !reflect.DeepEqual(actual, expected) {
				t.Errorf("%s: did not get correct event: %v", testCase.name, cmp.Diff(actual, expected, allowEventAndDate))
			}

			checkComments(client, testCase.name, testCase.expectedComment, t)
		})
	}
}

func TestBugKeyFromTitle(t *testing.T) {
	var testCases = []struct {
		title            string
		expectedKey      string
		expectedNotFound bool
		expectedIsBug    bool
	}{
		{
			title:            "no match",
			expectedKey:      "",
			expectedNotFound: true,
		},
		{
			title:         "OCPBUGS-12: Canonical",
			expectedKey:   "OCPBUGS-12",
			expectedIsBug: true,
		},
		{
			title:            "OCPBUGS-12 : Space before colon",
			expectedKey:      "",
			expectedNotFound: true,
		},
		{
			title:         "[rebase release-1.0] OCPBUGS-12: Prefix",
			expectedKey:   "OCPBUGS-12",
			expectedIsBug: true,
		},
		{
			title:         "Revert: \"OCPBUGS-12: Revert default\"",
			expectedKey:   "OCPBUGS-12",
			expectedIsBug: true,
		},
		{
			title:         "OCPBUGS-34: Revert: \"OCPBUGS-12: Revert default\"",
			expectedKey:   "OCPBUGS-34",
			expectedIsBug: true,
		},
		{
			title:       "[rebase release-1.0] JIRA-12: Prefix",
			expectedKey: "JIRA-12",
		},
		{
			title:       "JIRA-34: Revert: \"OCPBUGS-12: Revert default\"",
			expectedKey: "JIRA-34",
		},
		{
			title:         "OCPBUGS-12: Revert: \"JIRA-34: Revert default\"",
			expectedKey:   "OCPBUGS-12",
			expectedIsBug: true,
		},
		{
			title:       "No-issue: OCPBUGS-12: blah blah",
			expectedKey: "NO-JIRA",
		},
		{
			title:         "OCPBUGS-12: NO-ISSUE: blah blah",
			expectedKey:   "OCPBUGS-12",
			expectedIsBug: true,
		},
		{
			title:       "No-jira: OCPBUGS-12: blah blah",
			expectedKey: "NO-JIRA",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.title, func(t *testing.T) {
			key, notFound, isBug := jiraKeyFromTitle(testCase.title)
			if key != testCase.expectedKey {
				t.Errorf("%s: unexpected %s != %s", testCase.title, key, testCase.expectedKey)
			}
			if notFound != testCase.expectedNotFound {
				t.Errorf("%s: unexpected %t != %t", testCase.title, notFound, testCase.expectedNotFound)
			}
			if isBug != testCase.expectedIsBug {
				t.Errorf("%s: unexpected %t != %t", testCase.title, isBug, testCase.expectedIsBug)
			}
		})
	}
}

func TestBZIDFromTitle(t *testing.T) {
	var testCases = []struct {
		title            string
		expectedID       int
		expectedNotFound bool
	}{
		{
			title:            "no match",
			expectedID:       0,
			expectedNotFound: true,
		},
		{
			title:      "Bug 12: Canonical",
			expectedID: 12,
		},
		{
			title:            "Bug 12 : Space before colon",
			expectedID:       0,
			expectedNotFound: true,
		},
		{
			title:      "[rebase release-1.0] Bug 12: Prefix",
			expectedID: 12,
		},
		{
			title:      "Revert: \"Bug 12: Revert default\"",
			expectedID: 12,
		},
		{
			title:      "Bug 34: Revert: \"Bug 12: Revert default\"",
			expectedID: 34,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.title, func(t *testing.T) {
			key, notFound, err := bzIDFromTitle(testCase.title)
			if err != nil {
				t.Errorf("%s: unexpected error: %v", testCase.title, err)
			}
			if key != testCase.expectedID {
				t.Errorf("%s: unexpected %d != %d", testCase.title, key, testCase.expectedID)
			}
			if notFound != testCase.expectedNotFound {
				t.Errorf("%s: unexpected %t != %t", testCase.title, notFound, testCase.expectedNotFound)
			}
		})
	}
}

func TestValidateBug(t *testing.T) {
	open, closed := true, false
	oneStr, twoStr := "v1", "v2"
	one := []*jira.Version{{Name: "v1"}}
	two := []*jira.Version{{Name: "v2"}}
	verified := JiraBugState{Status: "VERIFIED"}
	modified := JiraBugState{Status: "MODIFIED"}
	updated := JiraBugState{Status: "UPDATED"}
	var testCases = []struct {
		name                    string
		issue                   *jira.Issue
		dependents              []dependent
		options                 JiraBranchOptions
		valid                   bool
		validations             []string
		why                     []string
		invalidDependentProject bool
	}{
		{
			name:    "no requirements means a valid bug",
			issue:   &jira.Issue{Fields: &jira.IssueFields{}},
			options: JiraBranchOptions{},
			valid:   true,
		},
		{
			name:        "matching open requirement means a valid bug",
			issue:       &jira.Issue{Fields: &jira.IssueFields{Status: &jira.Status{Name: "NEW"}}},
			options:     JiraBranchOptions{IsOpen: &open},
			valid:       true,
			validations: []string{"bug is open, matching expected state (open)"},
		},
		{
			name:        "matching closed requirement means a valid bug",
			issue:       &jira.Issue{Fields: &jira.IssueFields{Status: &jira.Status{Name: "CLOSED"}}},
			options:     JiraBranchOptions{IsOpen: &closed},
			valid:       true,
			validations: []string{"bug isn't open, matching expected state (not open)"},
		},
		{
			name:    "not matching open requirement means an invalid bug",
			issue:   &jira.Issue{Fields: &jira.IssueFields{Status: &jira.Status{Name: "CLOSED"}}},
			options: JiraBranchOptions{IsOpen: &open},
			valid:   false,
			why:     []string{"expected the bug to be open, but it isn't"},
		},
		{
			name:    "not matching closed requirement means an invalid bug",
			issue:   &jira.Issue{Fields: &jira.IssueFields{Status: &jira.Status{Name: "NEW"}}},
			options: JiraBranchOptions{IsOpen: &closed},
			valid:   false,
			why:     []string{"expected the bug to not be open, but it is"},
		},
		{
			name: "matching target version requirement means a valid bug",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Unknowns: tcontainer.MarshalMap{
					helpers.TargetVersionField: &one,
				},
			}},
			options:     JiraBranchOptions{TargetVersion: &oneStr},
			valid:       true,
			validations: []string{"bug target version (v1) matches configured target version for branch (v1)"},
		},
		{
			name: "not matching target version requirement means an invalid bug",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Unknowns: tcontainer.MarshalMap{
					helpers.TargetVersionField: &two,
				},
			}},
			options: JiraBranchOptions{TargetVersion: &oneStr},
			valid:   false,
			why:     []string{"expected the bug to target the \"v1\" version, but it targets \"v2\" instead"},
		},
		{
			name:    "not setting target version requirement means an invalid bug",
			issue:   &jira.Issue{Fields: &jira.IssueFields{}},
			options: JiraBranchOptions{TargetVersion: &oneStr},
			valid:   false,
			why:     []string{"expected the bug to target the \"v1\" version, but no target version was set"},
		},
		{
			name:        "matching status requirement means a valid bug",
			issue:       &jira.Issue{Fields: &jira.IssueFields{Status: &jira.Status{Name: "MODIFIED"}}},
			options:     JiraBranchOptions{ValidStates: &[]JiraBugState{modified}},
			valid:       true,
			validations: []string{"bug is in the state MODIFIED, which is one of the valid states (MODIFIED)"},
		},
		{
			name:        "matching status requirement means a valid bug (case-insensitive)",
			issue:       &jira.Issue{Fields: &jira.IssueFields{Status: &jira.Status{Name: "Modified"}}},
			options:     JiraBranchOptions{ValidStates: &[]JiraBugState{modified}},
			valid:       true,
			validations: []string{"bug is in the state Modified, which is one of the valid states (MODIFIED)"},
		},
		{
			name:        "matching status requirement by being in the migrated state means a valid bug",
			issue:       &jira.Issue{Fields: &jira.IssueFields{Status: &jira.Status{Name: "UPDATED"}}},
			options:     JiraBranchOptions{ValidStates: &[]JiraBugState{modified}, StateAfterValidation: &updated},
			valid:       true,
			validations: []string{"bug is in the state UPDATED, which is one of the valid states (MODIFIED, UPDATED)"},
		},
		{
			name:    "not matching status requirement means an invalid bug",
			issue:   &jira.Issue{Fields: &jira.IssueFields{Status: &jira.Status{Name: "MODIFIED"}}},
			options: JiraBranchOptions{ValidStates: &[]JiraBugState{verified}},
			valid:   false,
			why:     []string{"expected the bug to be in one of the following states: VERIFIED, but it is MODIFIED instead"},
		},
		{
			name:    "dependent status requirement with no dependent bugs means a valid bug",
			issue:   &jira.Issue{Key: "OCPBUGS-123", Fields: &jira.IssueFields{}},
			options: JiraBranchOptions{DependentBugStates: &[]JiraBugState{verified}},
			valid:   false,
			why:     []string{"expected [Jira Issue OCPBUGS-123](https://my-jira.com/browse/OCPBUGS-123) to depend on a bug in one of the following states: VERIFIED, but no dependents were found"},
		},
		{
			name:        "not matching dependent bug status requirement means an invalid bug",
			issue:       &jira.Issue{Fields: &jira.IssueFields{}},
			dependents:  []dependent{{key: "OCPBUGS-124", bugState: JiraBugState{Status: "MODIFIED"}}},
			options:     JiraBranchOptions{DependentBugStates: &[]JiraBugState{verified}},
			valid:       false,
			validations: []string{"bug has dependents"},
			why:         []string{"expected dependent [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124) to be in one of the following states: VERIFIED, but it is MODIFIED instead"},
		},
		{
			name:        "not matching dependent bug target version requirement means an invalid bug",
			issue:       &jira.Issue{Fields: &jira.IssueFields{}},
			dependents:  []dependent{{key: "OCPBUGS-124", bugState: JiraBugState{Status: "MODIFIED"}, targetVersion: &twoStr}},
			options:     JiraBranchOptions{DependentBugTargetVersions: &[]string{oneStr}},
			valid:       false,
			validations: []string{"bug has dependents"},
			why:         []string{"expected dependent [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124) to target a version in v1, but it targets \"v2\" instead"},
		},
		{
			name:        "not having a dependent bug target version means an invalid bug",
			issue:       &jira.Issue{Fields: &jira.IssueFields{}},
			dependents:  []dependent{{key: "OCPBUGS-124", bugState: JiraBugState{Status: "MODIFIED"}}},
			options:     JiraBranchOptions{DependentBugTargetVersions: &[]string{oneStr}},
			valid:       false,
			validations: []string{"bug has dependents"},
			why:         []string{"expected dependent [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124) to target a version in v1, but no target version was set"},
		},
		{
			name: "matching all requirements means a valid bug",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "MODIFIED"},
				Unknowns: tcontainer.MarshalMap{
					helpers.TargetVersionField: &one,
				},
			}},
			dependents: []dependent{{key: "OCPBUGS-124", bugState: JiraBugState{Status: "MODIFIED"}, targetVersion: &twoStr}},
			options:    JiraBranchOptions{IsOpen: &open, TargetVersion: &oneStr, ValidStates: &[]JiraBugState{modified}, DependentBugStates: &[]JiraBugState{modified}, DependentBugTargetVersions: &[]string{twoStr}},
			validations: []string{`bug is open, matching expected state (open)`,
				`bug target version (v1) matches configured target version for branch (v1)`,
				"bug is in the state MODIFIED, which is one of the valid states (MODIFIED)",
				"dependent bug [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124) is in the state MODIFIED, which is one of the valid states (MODIFIED)",
				`dependent [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124) targets the "v2" version, which is one of the valid target versions: v2`,
				"bug has dependents"},
			valid: true,
		},
		{
			name: "matching no requirements means an invalid bug",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status: &jira.Status{Name: "CLOSED"},
				Unknowns: tcontainer.MarshalMap{
					helpers.TargetVersionField: &one,
				},
			}},
			dependents:  []dependent{{key: "OCPBUGS-124", bugState: JiraBugState{Status: "MODIFIED"}, targetVersion: &twoStr}},
			options:     JiraBranchOptions{IsOpen: &open, TargetVersion: &twoStr, ValidStates: &[]JiraBugState{verified}, DependentBugStates: &[]JiraBugState{verified}},
			valid:       false,
			validations: []string{"bug has dependents"},
			why: []string{"expected the bug to be open, but it isn't",
				"expected the bug to target the \"v2\" version, but it targets \"v1\" instead",
				"expected the bug to be in one of the following states: VERIFIED, but it is CLOSED instead",
				"expected dependent [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124) to be in one of the following states: VERIFIED, but it is MODIFIED instead",
			},
		},
		{
			name: "matching status means a valid bug when resolution is not required",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "CLOSED"},
				Resolution: &jira.Resolution{Name: "LOL_GO_AWAY"},
			}},
			options:     JiraBranchOptions{ValidStates: &[]JiraBugState{{Status: "CLOSED"}}},
			valid:       true,
			validations: []string{"bug is in the state CLOSED (LOL_GO_AWAY), which is one of the valid states (CLOSED)"},
		},
		{
			name: "matching just status means an invalid bug when resolution does not match",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "CLOSED"},
				Resolution: &jira.Resolution{Name: "LOL_GO_AWAY"},
			}},
			options: JiraBranchOptions{ValidStates: &[]JiraBugState{{Status: "CLOSED", Resolution: "ERRATA"}}},
			valid:   false,
			why: []string{
				"expected the bug to be in one of the following states: CLOSED (ERRATA), but it is CLOSED (LOL_GO_AWAY) instead",
			},
		},
		{
			name: "matching status and resolution means a valid bug when both are required",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "CLOSED"},
				Resolution: &jira.Resolution{Name: "ERRATA"},
			}},
			options:     JiraBranchOptions{ValidStates: &[]JiraBugState{{Status: "CLOSED", Resolution: "ERRATA"}}},
			valid:       true,
			validations: []string{"bug is in the state CLOSED (ERRATA), which is one of the valid states (CLOSED (ERRATA))"},
		},
		{
			name: "matching status and resolution means a valid bug when both are required (case-insensitive)",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "Closed"},
				Resolution: &jira.Resolution{Name: "Errata"},
			}},
			options:     JiraBranchOptions{ValidStates: &[]JiraBugState{{Status: "CLOSED", Resolution: "ERRATA"}}},
			valid:       true,
			validations: []string{"bug is in the state Closed (Errata), which is one of the valid states (CLOSED (ERRATA))"},
		},
		{
			name: "matching resolution means a valid bug when status is not required",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "CLOSED"},
				Resolution: &jira.Resolution{Name: "ERRATA"},
			}},
			options:     JiraBranchOptions{ValidStates: &[]JiraBugState{{Resolution: "ERRATA"}}},
			valid:       true,
			validations: []string{"bug is in the state CLOSED (ERRATA), which is one of the valid states (any status with resolution ERRATA)"},
		},
		{
			name: "matching just resolution means an invalid bug when status does not match",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "CLOSED"},
				Resolution: &jira.Resolution{Name: "ERRATA"},
			}},
			options: JiraBranchOptions{ValidStates: &[]JiraBugState{{Status: "RESOLVED", Resolution: "ERRATA"}}},
			valid:   false,
			why: []string{
				"expected the bug to be in one of the following states: RESOLVED (ERRATA), but it is CLOSED (ERRATA) instead",
			},
		},
		{
			name: "matching status on dependent bug means a valid bug when resolution is not required",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "CLOSED"},
				Resolution: &jira.Resolution{Name: "LOL_GO_AWAY"},
			}},
			dependents:  []dependent{{key: "OCPBUGS-124", bugState: JiraBugState{Status: "CLOSED", Resolution: "LOL_GO_AWAY"}}},
			options:     JiraBranchOptions{DependentBugStates: &[]JiraBugState{{Status: "CLOSED"}}},
			valid:       true,
			validations: []string{"dependent bug [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124) is in the state CLOSED (LOL_GO_AWAY), which is one of the valid states (CLOSED)", "bug has dependents"},
		},
		{
			name: "matching just status on dependent bug means an invalid bug when resolution does not match",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "CLOSED"},
				Resolution: &jira.Resolution{Name: "LOL_GO_AWAY"},
			}},
			dependents:  []dependent{{key: "OCPBUGS-124", bugState: JiraBugState{Status: "CLOSED", Resolution: "LOL_GO_AWAY"}}},
			options:     JiraBranchOptions{DependentBugStates: &[]JiraBugState{{Status: "CLOSED", Resolution: "ERRATA"}}},
			valid:       false,
			validations: []string{"bug has dependents"},
			why: []string{
				"expected dependent [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124) to be in one of the following states: CLOSED (ERRATA), but it is CLOSED (LOL_GO_AWAY) instead",
			},
		},
		{
			name: "matching status and resolution on dependent bug means a valid bug when both are required",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "CLOSED"},
				Resolution: &jira.Resolution{Name: "ERRATA"},
			}},
			dependents:  []dependent{{key: "OCPBUGS-124", bugState: JiraBugState{Status: "CLOSED", Resolution: "ERRATA"}}},
			options:     JiraBranchOptions{DependentBugStates: &[]JiraBugState{{Status: "CLOSED", Resolution: "ERRATA"}}},
			valid:       true,
			validations: []string{"dependent bug [Jira Issue OCPBUGS-124](https://my-jira.com/browse/OCPBUGS-124) is in the state CLOSED (ERRATA), which is one of the valid states (CLOSED (ERRATA))", "bug has dependents"},
		},
		{
			name:        "valid states include the state after validation",
			issue:       &jira.Issue{Fields: &jira.IssueFields{Status: &jira.Status{Name: "MODIFIED"}}},
			options:     JiraBranchOptions{ValidStates: &[]JiraBugState{modified}, StateAfterValidation: &verified},
			valid:       true,
			validations: []string{"bug is in the state MODIFIED, which is one of the valid states (MODIFIED, VERIFIED)"},
		},
		{
			name:        "valid states include the state after validation, but does not duplicate it",
			issue:       &jira.Issue{Fields: &jira.IssueFields{Status: &jira.Status{Name: "MODIFIED"}}},
			options:     JiraBranchOptions{ValidStates: &[]JiraBugState{modified, verified}, StateAfterValidation: &verified},
			valid:       true,
			validations: []string{"bug is in the state MODIFIED, which is one of the valid states (MODIFIED, VERIFIED)"},
		},
		{
			name: "dependent bug not being in OCPBUGS project results in failure",
			issue: &jira.Issue{Fields: &jira.IssueFields{
				Status:     &jira.Status{Name: "CLOSED"},
				Resolution: &jira.Resolution{Name: "ERRATA"},
			}},
			dependents:  []dependent{{key: "OCPBUGSM-38676", bugState: JiraBugState{Status: "CLOSED", Resolution: "ERRATA"}}},
			options:     JiraBranchOptions{DependentBugStates: &[]JiraBugState{{Status: "CLOSED", Resolution: "ERRATA"}}},
			valid:       false,
			validations: []string{"bug has dependents"},
			why: []string{
				"bug has dependents",
				"dependent bug OCPBUGSM-38676 is not in the required `OCPBUGS` project",
			},
			invalidDependentProject: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			valid, invalidDependentProject, validations, why := validateBug(testCase.issue, testCase.dependents, testCase.options, "https://my-jira.com", "https://my-bugzilla.com")
			if valid != testCase.valid {
				t.Errorf("%s: didn't validate bug correctly, expected %t got %t", testCase.name, testCase.valid, valid)
			}
			if !reflect.DeepEqual(validations, testCase.validations) {
				t.Errorf("%s: didn't get correct validations: %v", testCase.name, cmp.Diff(testCase.validations, validations, allowEventAndDate))
			}
			if !reflect.DeepEqual(why, testCase.why) {
				t.Errorf("%s: didn't get correct reasons why: %v", testCase.name, cmp.Diff(testCase.why, why, allowEventAndDate))
			}
			if invalidDependentProject != testCase.invalidDependentProject {
				t.Errorf("%s: didn't get correct dependent bug project validation, expected %t got %t", testCase.name, testCase.invalidDependentProject, invalidDependentProject)
			}
		})
	}
}

func TestProcessQuery(t *testing.T) {
	var testCases = []struct {
		name     string
		query    emailToLoginQuery
		email    string
		expected string
	}{
		{
			name: "single login returns cc",
			query: emailToLoginQuery{
				Search: querySearch{
					Edges: []queryEdge{{
						Node: queryNode{
							User: queryUser{
								Login: "ValidLogin",
							},
						},
					}},
				},
			},
			email:    "qa_tester@example.com",
			expected: "Requesting review from QA contact:\n/cc @ValidLogin",
		}, {
			name: "no login returns not found error",
			query: emailToLoginQuery{
				Search: querySearch{
					Edges: []queryEdge{},
				},
			},
			email:    "qa_tester@example.com",
			expected: "No GitHub users were found matching the public email listed for the QA contact in Jira (qa_tester@example.com), skipping review request.",
		}, {
			name: "multiple logins returns multiple results error",
			query: emailToLoginQuery{
				Search: querySearch{
					Edges: []queryEdge{{
						Node: queryNode{
							User: queryUser{
								Login: "Login1",
							},
						},
					}, {
						Node: queryNode{
							User: queryUser{
								Login: "Login2",
							},
						},
					}},
				},
			},
			email:    "qa_tester@example.com",
			expected: "Multiple GitHub users were found matching the public email listed for the QA contact in Jira (qa_tester@example.com), skipping review request. List of users with matching email:\n\t- Login1\n\t- Login2",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			response := processQuery(&testCase.query, testCase.email, logrus.WithField("testCase", testCase.name))
			if response != testCase.expected {
				t.Errorf("%s: Expected \"%s\", got \"%s\"", testCase.name, testCase.expected, response)
			}
		})
	}
}

func TestGetCherrypickPRMatch(t *testing.T) {
	var prNum = 123
	var branch = "v2"
	var testCases = []struct {
		name      string
		requestor string
		note      string
	}{{
		name: "No requestor or string",
	}, {
		name:      "Include requestor",
		requestor: "user",
	}, {
		name: "Include note",
		note: "this is a test",
	}, {
		name:      "Include requestor and note",
		requestor: "user",
		note:      "this is a test",
	}}
	var pr = &github.PullRequestEvent{
		PullRequest: github.PullRequest{
			Base: github.PullRequestBranch{
				Ref: branch,
			},
		},
	}
	for _, testCase := range testCases {
		testPR := *pr
		testPR.PullRequest.Body = cherrypicker.CreateCherrypickBody(prNum, testCase.requestor, testCase.note)
		cherrypick, cherrypickOfPRNum, err := getCherryPickMatch(testPR)
		if err != nil {
			t.Fatalf("%s: Got error but did not expect one: %v", testCase.name, err)
		}
		if !cherrypick {
			t.Errorf("%s: Expected cherrypick to be true, but got false", testCase.name)
		}
		if cherrypickOfPRNum != prNum {
			t.Errorf("%s: Got incorrect PR num: Expected %d, got %d", testCase.name, prNum, cherrypickOfPRNum)
		}
	}
}

func TestIsBugAllowed(t *testing.T) {
	testCases := []struct {
		name           string
		bug            *jira.Issue
		securityLevels []string
		expected       bool
	}{
		{
			name:           "no groups configured means always allowed",
			securityLevels: []string{},
			expected:       true,
		},
		{
			name: "matching one level is allowed",
			bug: &jira.Issue{Fields: &jira.IssueFields{
				Unknowns: tcontainer.MarshalMap{
					"security": jiraclient.SecurityLevel{Name: "whoa"},
				},
			}},
			securityLevels: []string{"whoa", "really", "cool"},
			expected:       true,
		},
		{
			name: "no levels matching is not allowed",
			bug: &jira.Issue{Fields: &jira.IssueFields{
				Unknowns: tcontainer.MarshalMap{
					"security": jiraclient.SecurityLevel{Name: "whoa"},
				},
			}},
			securityLevels: []string{"other"},
			expected:       false,
		},
		{
			name:           "no level set in bug is equal to level default",
			bug:            &jira.Issue{Fields: &jira.IssueFields{}},
			securityLevels: []string{"default"},
			expected:       true,
		},
		{
			name:           "default level is not set",
			bug:            &jira.Issue{Fields: &jira.IssueFields{}},
			securityLevels: []string{"internal"},
			expected:       false,
		},
	}
	for _, testCase := range testCases {
		actual, err := isBugAllowed(testCase.bug, testCase.securityLevels)
		if err != nil {
			// this error should never occur when run against a real jira server, so no need to test error handling
			t.Fatalf("%s: unexpected error: %v", testCase.name, err)
		}
		if actual != testCase.expected {
			t.Errorf("%s: isBugAllowed returned %v incorrectly", testCase.name, actual)
		}
	}
}
