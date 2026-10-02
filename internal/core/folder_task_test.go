package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskServiceCreateTask_OutsideGitRunsTheTaskInTheFolderAsItIs(t *testing.T) {
	svc := newTestTaskService(t)
	svc.repoClient.outsideWorktree = true
	svc.providerRepo.suggestedName = "delegated login fixes"

	task, err := svc.service.CreateTaskWithProgress(t.Context(), CreateTaskInput{
		Cwd:    "/src/licentiam/code",
		Prompt: "fix delegated logins across api and portals",
	}, nil)

	require.NoError(t, err)
	require.Equal(t, WorkspaceKindFolder, task.WorkspaceKind)
	require.Equal(t, "/src/licentiam/code", task.WorktreePath)
	require.Equal(t, "/src/licentiam/code", task.RepoRoot)
	require.Equal(t, "code", task.RepoName)
	require.Empty(t, task.BranchName)
	require.Equal(t, "code_delegated-login-fixes", task.TmuxSession)
	require.Nil(t, svc.repoClient.createdTask, "a folder task must not create a worktree")
	require.False(t, svc.workspace.setupCalled, "a folder task must not be seeded")
	require.True(t, svc.workspace.bootstrapCalled, "provider hooks still need registering")
	require.Equal(t, "/src/licentiam/code", svc.workspace.worktreePath)
	require.Equal(t, "code_delegated-login-fixes", svc.sessionClient.startedTask.TmuxSession)
	require.Equal(t, TaskCreationStatusReady, svc.taskRepo.updatedTask.CreationStatus)
}

func TestTaskServiceCreateTask_FolderWorkspaceCanBeRequestedInsideARepository(t *testing.T) {
	svc := newTestTaskService(t)
	svc.providerRepo.suggestedName = "triage flaky specs"

	task, err := svc.service.CreateTaskWithProgress(t.Context(), CreateTaskInput{
		Cwd:       "/src/api",
		Prompt:    "triage flaky specs",
		Workspace: WorkspaceKindFolder,
	}, nil)

	require.NoError(t, err)
	require.True(t, task.UsesFolderWorkspace())
	require.Equal(t, "/src/api", task.WorktreePath)
	require.Nil(t, svc.repoClient.createdTask)
}

func TestTaskServiceCreateTask_FolderTasksAvoidSessionNamesOfSameNamedFolders(t *testing.T) {
	svc := newTestTaskService(t)
	svc.repoClient.outsideWorktree = true
	svc.providerRepo.suggestedName = "fix tests"
	svc.taskRepo.listTasks = []*Task{{
		ID:            "task-wef",
		Slug:          "fix-tests",
		DisplayName:   "fix tests",
		RepoRoot:      "/src/wef/code",
		RepoName:      "code",
		WorktreePath:  "/src/wef/code",
		TmuxSession:   "code_fix-tests",
		WorkspaceKind: WorkspaceKindFolder,
	}}

	task, err := svc.service.CreateTaskWithProgress(t.Context(), CreateTaskInput{
		Cwd:    "/src/licentiam/code",
		Prompt: "fix tests",
	}, nil)

	require.NoError(t, err)
	require.NotEqual(t, "code_fix-tests", task.TmuxSession)
}

func TestTaskServiceCreateTask_FolderTaskNeedsAnAbsoluteFolder(t *testing.T) {
	svc := newTestTaskService(t)
	svc.repoClient.outsideWorktree = true

	_, err := svc.service.CreateTaskWithProgress(t.Context(), CreateTaskInput{
		Cwd:    "relative/dir",
		Prompt: "anything",
	}, nil)

	require.ErrorContains(t, err, "absolute working directory")
}

func TestTaskService_DeleteFolderTaskKeepsTheFolder(t *testing.T) {
	svc := newTestTaskService(t)
	svc.taskRepo.listTasks = []*Task{{
		ID:            "task-1",
		Slug:          "delegated-login-fixes",
		RepoRoot:      "/src/licentiam/code",
		WorktreePath:  "/src/licentiam/code",
		TmuxSession:   "code_delegated-login-fixes",
		WorkspaceKind: WorkspaceKindFolder,
	}}

	err := svc.service.DeleteTask(t.Context(), "task-1")

	require.NoError(t, err)
	require.NotNil(t, svc.sessionClient.deletedTask)
	require.Nil(t, svc.repoClient.removedTask, "the folder belongs to the user, not the task")
	require.Equal(t, "task-1", svc.taskRepo.deletedTaskID)
}

func TestTaskStatusService_HandleHookEventNeverMatchesAFolderTaskByDirectory(t *testing.T) {
	svc := newTestTaskService(t)
	svc.taskRepo.listTasks = []*Task{{
		ID:            "task-1",
		WorktreePath:  "/src/licentiam/code",
		WorkspaceKind: WorkspaceKindFolder,
	}}

	// A provider session the user started in the same folder outside Rig.
	err := svc.service.HandleHookEvent(t.Context(), HookEventInput{
		Provider:  ProviderCodex,
		Cwd:       "/src/licentiam/code",
		EventName: "SessionStart",
	})
	require.ErrorIs(t, err, ErrUnmanagedHookEvent)

	err = svc.service.HandleHookEvent(t.Context(), HookEventInput{
		Provider:  ProviderCodex,
		TaskID:    "task-1",
		Cwd:       "/src/licentiam/code",
		EventName: "SessionStart",
	})
	require.NoError(t, err)
	require.Equal(t, "task-1", svc.providerRepo.hookInput.TaskID)
}
