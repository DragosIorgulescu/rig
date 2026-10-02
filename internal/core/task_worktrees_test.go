package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type worktreesHarness struct {
	worktrees *taskWorktrees
	tasks     *MockTaskRepository
	git       *MockGitWorktreeClient
	claude    *MockProviderClient
	upserted  []TaskWorktreeRecord
}

func newWorktreesHarness(t *testing.T, changes []SessionFileChange, records []TaskWorktreeRecord) *worktreesHarness {
	t.Helper()
	h := &worktreesHarness{
		tasks:  NewMockTaskRepository(t),
		git:    NewMockGitWorktreeClient(t),
		claude: NewMockProviderClient(t),
	}
	h.worktrees = newTaskWorktrees(h.tasks, h.git, map[Provider]ProviderClient{ProviderClaude: h.claude})

	session := TaskProviderSession{
		TaskID:            "task-1",
		Provider:          ProviderClaude,
		ProviderSessionID: "sess-1",
		TranscriptPath:    "/transcripts/sess-1.jsonl",
	}
	h.tasks.EXPECT().ListTaskProviderSessions(mock.Anything, "task-1").Return([]TaskProviderSession{session}, nil)
	h.claude.EXPECT().ReadSessionFileChanges(mock.Anything, session).Return(changes, nil)
	h.tasks.EXPECT().ListTaskWorktreeRecords(mock.Anything, "task-1").Return(records, nil).Maybe()
	h.tasks.EXPECT().UpsertTaskWorktreeRecord(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, record TaskWorktreeRecord) error {
			h.upserted = append(h.upserted, record)
			return nil
		},
	).Maybe()
	return h
}

func (h *worktreesHarness) worktree(root string, repo string, branch string, dirs ...string) {
	for _, dir := range dirs {
		h.git.EXPECT().WorktreeRootOf(dir).Return(root).Once()
	}
	h.git.EXPECT().InspectWorktree(mock.Anything, root).
		Return(&WorktreeRef{Root: root, RepoName: repo, Branch: branch}, nil).
		Maybe()
}

func at(minute int) time.Time {
	return time.Date(2026, time.October, 2, 9, minute, 0, 0, time.UTC)
}

func TestTaskWorktrees_GroupsEditsByWorktreeAndPinsTheirCurrentBranch(t *testing.T) {
	h := newWorktreesHarness(t, []SessionFileChange{
		{ObservedAt: at(1), Path: "/src/api-pdc/app/models/budget.rb"},
		{ObservedAt: at(2), Path: "/src/portals-pdc/apps/admin/routes.tsx"},
		{ObservedAt: at(3), Path: "/src/api-pdc/app/models/alert.rb"},
	}, nil)
	h.worktree("/src/api-pdc", "api", "fix/budget-alert", "/src/api-pdc/app/models")
	h.worktree("/src/portals-pdc", "portals", "feat/pdc", "/src/portals-pdc/apps/admin")

	worktrees, err := h.worktrees.ListTaskWorktrees(t.Context(), "task-1")

	require.NoError(t, err)
	require.Equal(t, []TaskWorktree{
		{
			LastEditAt:   at(3),
			WorktreePath: "/src/api-pdc",
			RepoName:     "api",
			Branch:       "fix/budget-alert",
			EditedBranch: "fix/budget-alert",
			EditCount:    2,
		},
		{
			LastEditAt:   at(2),
			WorktreePath: "/src/portals-pdc",
			RepoName:     "portals",
			Branch:       "feat/pdc",
			EditedBranch: "feat/pdc",
			EditCount:    1,
		},
	}, worktrees)
	require.ElementsMatch(t, []TaskWorktreeRecord{
		{LastEditAt: at(3), TaskID: "task-1", WorktreePath: "/src/api-pdc", RepoName: "api",
			Branch: "fix/budget-alert", EditCount: 2},
		{LastEditAt: at(2), TaskID: "task-1", WorktreePath: "/src/portals-pdc", RepoName: "portals",
			Branch: "feat/pdc", EditCount: 1},
	}, h.upserted)
}

func TestTaskWorktrees_FlagsAWorktreeThatMovedToAnotherBranchSinceTheLastEdit(t *testing.T) {
	h := newWorktreesHarness(t, []SessionFileChange{
		{ObservedAt: at(5), Path: "/src/portals-1/src/main.tsx"},
	}, []TaskWorktreeRecord{{
		LastEditAt: at(5), TaskID: "task-1", WorktreePath: "/src/portals-1", RepoName: "portals",
		Branch: "fix/avatar", EditCount: 1,
	}})
	h.worktree("/src/portals-1", "portals", "feat/deep-links", "/src/portals-1/src")

	worktrees, err := h.worktrees.ListTaskWorktrees(t.Context(), "task-1")

	require.NoError(t, err)
	require.Len(t, worktrees, 1)
	require.Equal(t, "feat/deep-links", worktrees[0].Branch)
	require.Equal(t, "fix/avatar", worktrees[0].EditedBranch)
	require.True(t, worktrees[0].BranchChanged())
	require.Empty(t, h.upserted, "no new edit, so the recorded branch must not move")
}

func TestTaskWorktrees_ANewEditAfterABranchSwitchAdoptsTheNewBranch(t *testing.T) {
	h := newWorktreesHarness(t, []SessionFileChange{
		{ObservedAt: at(5), Path: "/src/portals-1/src/main.tsx"},
		{ObservedAt: at(9), Path: "/src/portals-1/src/app.tsx"},
	}, []TaskWorktreeRecord{{
		LastEditAt: at(5), TaskID: "task-1", WorktreePath: "/src/portals-1", RepoName: "portals",
		Branch: "fix/avatar", EditCount: 1,
	}})
	h.worktree("/src/portals-1", "portals", "feat/deep-links", "/src/portals-1/src")

	worktrees, err := h.worktrees.ListTaskWorktrees(t.Context(), "task-1")

	require.NoError(t, err)
	require.Len(t, worktrees, 1)
	require.False(t, worktrees[0].BranchChanged())
	require.Equal(t, "feat/deep-links", worktrees[0].EditedBranch)
	require.Len(t, h.upserted, 1)
	require.Equal(t, "feat/deep-links", h.upserted[0].Branch)
	require.Equal(t, at(9), h.upserted[0].LastEditAt)
}

func TestTaskWorktrees_DropsEditsOutsideAnyExistingWorktree(t *testing.T) {
	h := newWorktreesHarness(t, []SessionFileChange{
		{ObservedAt: at(1), Path: "/tmp/scratch/notes.md"},
		{ObservedAt: at(2), Path: "/src/api-removed/app/models/budget.rb"},
		{ObservedAt: at(3), Path: "/src/api-stale/app.rb"},
	}, nil)
	h.git.EXPECT().WorktreeRootOf("/tmp/scratch").Return("")
	h.git.EXPECT().WorktreeRootOf("/src/api-removed/app/models").Return("")
	h.git.EXPECT().WorktreeRootOf("/src/api-stale").Return("/src/api-stale")
	h.git.EXPECT().InspectWorktree(mock.Anything, "/src/api-stale").Return(nil, nil)

	worktrees, err := h.worktrees.ListTaskWorktrees(t.Context(), "task-1")

	require.NoError(t, err)
	require.Empty(t, worktrees)
	require.Empty(t, h.upserted)
}
