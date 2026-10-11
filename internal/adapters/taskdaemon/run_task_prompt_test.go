package taskdaemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/BaronBonet/rig/internal/core"

	"github.com/stretchr/testify/require"
)

func TestRunTaskPromptRoundTrip(t *testing.T) {
	t.Parallel()

	svc := newFakeTaskService()
	svc.promptRunResult = core.RunTaskPromptResult{Outcome: core.PromptRunLaunched, Detail: "resumed 3f2a…"}
	client := startTestFrontend(t, svc)

	result, err := client.RunTaskPrompt(context.Background(), core.RunTaskPromptInput{
		TaskID:         "task-1",
		Prompt:         "- triage the issues\nthen summarise",
		SessionMode:    core.PromptRunSessionSame,
		PermissionMode: core.TaskPermissionModeAcceptEdits,
	})

	require.NoError(t, err)
	require.Equal(t, svc.promptRunResult, result)
	svc.mu.Lock()
	require.Equal(t, &core.RunTaskPromptInput{
		TaskID:         "task-1",
		Prompt:         "- triage the issues\nthen summarise",
		SessionMode:    core.PromptRunSessionSame,
		PermissionMode: core.TaskPermissionModeAcceptEdits,
	}, svc.promptRunInput, "every field reaches the service")
	svc.mu.Unlock()
}

func TestRunTaskPromptRoundTrip_ASkipIsAResultNotAnError(t *testing.T) {
	t.Parallel()

	svc := newFakeTaskService()
	svc.promptRunResult = core.RunTaskPromptResult{Outcome: core.PromptRunSkipped, Detail: "skipped: still working"}
	client := startTestFrontend(t, svc)

	result, err := client.RunTaskPrompt(context.Background(), core.RunTaskPromptInput{TaskID: "task-1", Prompt: "go"})

	require.NoError(t, err)
	require.Equal(t, core.PromptRunSkipped, result.Outcome)
	require.Equal(t, "skipped: still working", result.Detail)
}

func TestRunTaskPromptRoundTrip_CarriesServiceErrors(t *testing.T) {
	t.Parallel()

	svc := newFakeTaskService()
	svc.errByOp["run_task_prompt"] = errors.New("start task session: tmux refused")
	client := startTestFrontend(t, svc)

	_, err := client.RunTaskPrompt(context.Background(), core.RunTaskPromptInput{TaskID: "task-1", Prompt: "go"})

	require.ErrorContains(t, err, "tmux refused")
}

func TestRunTaskPromptRoundTrip_RequiresATaskID(t *testing.T) {
	t.Parallel()

	svc := newFakeTaskService()
	client := startTestFrontend(t, svc)

	_, err := client.RunTaskPrompt(context.Background(), core.RunTaskPromptInput{Prompt: "go"})

	require.ErrorContains(t, err, "run_task_prompt task_id required")
	svc.mu.Lock()
	require.Nil(t, svc.promptRunInput, "the service is not asked")
	svc.mu.Unlock()
}

func TestRunTaskPromptOperation_HasTheDocumentedWireShape(t *testing.T) {
	t.Parallel()

	require.Equal(t, "run_task_prompt", opRunTaskPrompt.command)
	require.Contains(t, socketUnaryHandlers, "run_task_prompt")

	request, err := json.Marshal(runTaskPromptRequest{
		TaskID:         "task-1",
		Prompt:         "go",
		SessionMode:    core.PromptRunSessionNew,
		PermissionMode: core.TaskPermissionModeBypass,
	})
	require.NoError(t, err)
	require.JSONEq(t,
		`{"task_id":"task-1","prompt":"go","session_mode":"new","permission_mode":"bypass"}`,
		string(request))

	response, err := json.Marshal(core.RunTaskPromptResult{Outcome: core.PromptRunLaunched, Detail: "started fresh"})
	require.NoError(t, err)
	require.JSONEq(t, `{"outcome":"launched","detail":"started fresh"}`, string(response))
}
