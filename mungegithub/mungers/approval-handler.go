/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mungers

import (
	"strings"

	"k8s.io/contrib/mungegithub/features"
	"k8s.io/contrib/mungegithub/github"
	c "k8s.io/contrib/mungegithub/mungers/matchers/comment"
	"k8s.io/kubernetes/pkg/util/sets"

	"bytes"
	"fmt"
	"sort"

	"github.com/golang/glog"
	githubapi "github.com/google/go-github/github"
	"github.com/spf13/cobra"
)

const (
	approvalNotificationName = "ApprovalNotifier"
	approveCommand           = "approve"
)

// ApprovalHandler will try to add "approved" label once
// all files of change has been approved by approvers.
type ApprovalHandler struct {
	features *features.Features
}

func init() {
	h := &ApprovalHandler{}
	RegisterMungerOrDie(h)
}

// Name is the name usable in --pr-mungers
func (*ApprovalHandler) Name() string { return "approval-handler" }

// RequiredFeatures is a slice of 'features' that must be provided
func (*ApprovalHandler) RequiredFeatures() []string {
	return []string{features.RepoFeatureName, features.AliasesFeature}
}

// Initialize will initialize the munger
func (h *ApprovalHandler) Initialize(config *github.Config, features *features.Features) error {
	h.features = features
	return nil
}

// EachLoop is called at the start of every munge loop
func (*ApprovalHandler) EachLoop() error { return nil }

// AddFlags will add any request flags to the cobra `cmd`
func (*ApprovalHandler) AddFlags(cmd *cobra.Command, config *github.Config) {}

// Munge is the workhorse the will actually make updates to the PR
func (h *ApprovalHandler) Munge(obj *github.MungeObject) {
	if !obj.IsPR() {
		return
	}
	files, err := obj.ListFiles()
	if err != nil {
		glog.Errorf("failed to list files in this PR: %v", err)
		return
	}

	comments, err := getCommentsAfterLastModified(obj)
	if err != nil {
		glog.Errorf("failed to get comments in this PR: %v", err)
		return
	}

	// Go through all comments after latest commit.
	//  If anyone said "/approve", add them to approverSet.
	//  If anyone said "/approve cancel", remove them
	approverSet := createApproverSet(comments)

	// map from OWNERS pathnames to a set of users who approved at that level.
	ownersMap := h.getApprovedOwners(files, approverSet)

	if err := h.updateNotification(obj, ownersMap); err != nil {
		return
	}

	for _, approverSet := range ownersMap {
		if approverSet.Len() == 0 {
			if obj.HasLabel(approvedLabel) {
				obj.RemoveLabel(approvedLabel)
			}
			return
		}
	}

	if !obj.HasLabel(approvedLabel) {
		obj.AddLabel(approvedLabel)
	}
}

func (h *ApprovalHandler) updateNotification(obj *github.MungeObject, ownersMap map[string]sets.String) error {
	comments, err := obj.ListComments()
	if err != nil {
		return err
	}

	notificationMatcher := c.MungerNotificationName(approvalNotificationName)
	notifications := c.FilterComments(comments, notificationMatcher)
	latestNotification := notifications.GetLast()
	if latestNotification == nil {
		return h.createMessage(obj, ownersMap)
	}

	approveMatcher := c.CommandName(approveCommand)
	approveComments := c.FilterComments(comments, approveMatcher)
	latestApprove := approveComments.GetLast()
	if latestApprove == nil {
		// there was already a bot notification and nothing has changed since
		return nil
	}

	if latestApprove.CreatedAt == nil || latestNotification.CreatedAt == nil {
		// API gave us crap, nothing we can do
		return nil
	}

	if latestApprove.CreatedAt.After(*latestNotification.CreatedAt) {
		// there has been approval since last notification
		obj.DeleteComment(latestNotification)
		return h.createMessage(obj, ownersMap)
	}

	lastModified := obj.LastModifiedTime()
	if latestNotification.CreatedAt.Before(*lastModified) {
		obj.DeleteComment(latestNotification)
		return h.createMessage(obj, ownersMap)
	}
	return nil
}

// findApproverSet Takes all the Owners Files that Are Needed for the PR and chooses a good
// subset of Approvers that are guaranteed to be from all of them (exact cover)
// This is a greedy approximation and not guaranteed to find the minimum number of OWNERS
func (h ApprovalHandler) findApproverSet(ownersPath sets.String) sets.String {

	// approverCount contains a map: person -> set of relevant OWNERS file they are in
	approverCount := map[string]sets.String{}
	for ownersFile := range ownersPath {
		for approver := range h.features.Repos.LeafApprovers(ownersFile) {
			if _, ok := approverCount[approver]; !ok {
				approverCount[approver] = sets.NewString()
			}
			approverCount[approver].Insert(ownersFile)
		}
	}

	copyOfFiles := sets.NewString()
	for fn := range ownersPath {
		copyOfFiles.Insert(fn)
	}

	approverGroup := sets.NewString()
	for copyOfFiles.Len() > 0 {
		maxCovered := 0
		var bestPerson string
		for k, v := range approverCount {
			covered := v.Intersection(copyOfFiles)
			if covered.Len() > maxCovered {
				maxCovered = len(v)
				bestPerson = k
			}
		}

		approverGroup.Insert(bestPerson)
		coveredFiles := approverCount[bestPerson]
		copyOfFiles.Delete(coveredFiles.List()...)
	}
	return approverGroup
}

func (h *ApprovalHandler) createMessage(obj *github.MungeObject, ownersMap map[string]sets.String) error {
	// sort the keys so we always display OWNERS files in same order
	OWNERSFiles := make([]string, len(ownersMap))
	i := 0
	for path := range ownersMap {
		OWNERSFiles[i] = path
		i++
	}
	sort.Strings(OWNERSFiles)

	unapprovedOwners := sets.NewString()
	context := bytes.NewBufferString("")
	for _, path := range OWNERSFiles {
		approverSet := ownersMap[path]
		if approverSet.Len() == 0 {
			context.WriteString(fmt.Sprintf("- **%s**\n", path))
			unapprovedOwners.Insert(path)
		} else {
			context.WriteString(fmt.Sprintf("- ~~%s~~ [%v]\n", path, strings.Join(approverSet.List(), ",")))
		}
	}
	context.WriteString("\n")
	if unapprovedOwners.Len() > 0 {
		context.WriteString("We suggest the following people:\n")
		context.WriteString("cc ")
		toBeAssigned := h.findApproverSet(unapprovedOwners)
		for person := range toBeAssigned {
			context.WriteString("@" + person + " ")
		}
	}
	context.WriteString("\n You can indicate your approval by writing `/approve` in a comment")
	context.WriteString("\n You can cancel your approval by writing `/approve cancel`in a comment")

	notification := c.Notification{
		Name:      approvalNotificationName,
		Arguments: "The Following OWNERS Files Need Approval:\n",
		Context:   context.String(),
	}
	return notification.Post(obj)
}

// createApproverSet iterates through the list of comments on a PR
// and identifies all of the people that have said /approve and adds
// them to the approverSet.  The function uses the latest approve or cancel comment
// to determine the Users intention
func createApproverSet(comments []*githubapi.IssueComment) sets.String {
	approverSet := sets.NewString()

	approverMatcher := c.CommandName(approveCommand)
	for _, comment := range c.FilterComments(comments, approverMatcher) {
		cmd := c.ParseCommand(comment)
		if cmd.Arguments == "cancel" {
			approverSet.Delete(*comment.User.Login)
		} else {
			approverSet.Insert(*comment.User.Login)
		}
	}
	return approverSet
}

// getApprovedOwners finds all the relevant OWNERS files for the PRs and identifies all the people from them
// that have approved the PR
//
// ex:
// approverSet contains "rootApprover", "pkgApprover", and "randomGuy"
// /OWNERS contains "rootApprover"
// /pkg/OWNERS contains "pkgApprover"
// The files /pkg/file.go and /api/file.go have been changed.
//
// This will return:
// map[string]sets.String {
//	"/":     [rootApprover]
//	"/pkg/": [rootApprover, pkgApprover]
// }
func (h ApprovalHandler) getApprovedOwners(files []*githubapi.CommitFile, approverSet sets.String) map[string]sets.String {
	ownersApprovers := map[string]sets.String{}
	for _, file := range files {
		filename := *file.Filename

		OWNERSFilename := h.features.Repos.OWNERSPathForFile(filename)
		if _, ok := ownersApprovers[OWNERSFilename]; ok {
			// something else already figure it out
			continue
		}

		fileApprovers := h.features.Repos.Approvers(filename)
		fileApprovers = fileApprovers.Intersection(approverSet)

		ownersApprovers[OWNERSFilename] = fileApprovers
	}
	return ownersApprovers
}

func getCommentsAfterLastModified(obj *github.MungeObject) ([]*githubapi.IssueComment, error) {
	afterLastModified := func(opt *githubapi.IssueListCommentsOptions) *githubapi.IssueListCommentsOptions {
		// Only comments updated at or after this time are returned.
		// One possible case is that reviewer might "/lgtm" first, contributor updated PR, and reviewer updated "/lgtm".
		// This is still valid. We don't recommend user to update it.
		lastModified := *obj.LastModifiedTime()
		opt.Since = lastModified
		return opt
	}
	return obj.ListComments(afterLastModified)
}
