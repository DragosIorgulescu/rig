package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/BaronBonet/rig/internal/core"
)

// runScheduleCommand runs `rig <args>` against a runtime that records the
// request `schedule try` hands over.
func runScheduleCommand(t *testing.T, args ...string) (*scheduleTryRequest, error) {
	t.Helper()

	var got *scheduleTryRequest
	cmd := newRootCommand(commandRuntime{
		version: "test-version",
		runScheduleTry: func(request scheduleTryRequest) error {
			got = &request
			return nil
		},
	})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	return got, err
}

func TestScheduleTry_ParsesATaskAndAPrompt(t *testing.T) {
	t.Parallel()

	got, err := runScheduleCommand(t, "schedule", "try", "billing-retry-flow", "triage the open issues")

	require.NoError(t, err)
	require.Equal(t, &scheduleTryRequest{
		Task:           "billing-retry-flow",
		Prompt:         "triage the open issues",
		SessionMode:    core.PromptRunSessionNew,
		PermissionMode: core.TaskPermissionModeAuto,
	}, got, "a new session in auto mode unless told otherwise")
}

func TestScheduleTry_ParsesTheModeAndPermissionFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		flags      []string
		mode       core.PromptRunSessionMode
		permission core.TaskPermissionMode
	}{
		{[]string{"--mode", "new"}, core.PromptRunSessionNew, core.TaskPermissionModeAuto},
		{[]string{"--mode", "same"}, core.PromptRunSessionSame, core.TaskPermissionModeAuto},
		{[]string{"--permission", "auto"}, core.PromptRunSessionNew, core.TaskPermissionModeAuto},
		{[]string{"--permission", "default"}, core.PromptRunSessionNew, core.TaskPermissionModeDefault},
		{[]string{"--permission", "accept-edits"}, core.PromptRunSessionNew, core.TaskPermissionModeAcceptEdits},
		{[]string{"--permission", "bypass"}, core.PromptRunSessionNew, core.TaskPermissionModeBypass},
		{
			[]string{"--mode", "same", "--permission", "bypass"},
			core.PromptRunSessionSame, core.TaskPermissionModeBypass,
		},
	}
	for _, tt := range tests {
		t.Run(joinArgs(tt.flags), func(t *testing.T) {
			args := append([]string{"schedule", "try", "task-1", "go"}, tt.flags...)

			got, err := runScheduleCommand(t, args...)

			require.NoError(t, err)
			require.Equal(t, tt.mode, got.SessionMode)
			require.Equal(t, tt.permission, got.PermissionMode)
		})
	}
}

func joinArgs(args []string) string {
	joined := ""
	for _, arg := range args {
		joined += arg + " "
	}
	return joined
}

func TestScheduleTry_ReadsThePromptFromAFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "prompt.txt")
	require.NoError(t, os.WriteFile(path, []byte("- review the PRs\nthen report\n"), 0o600))

	got, err := runScheduleCommand(t, "schedule", "try", "task-1", "--prompt-file", path)

	require.NoError(t, err)
	require.Equal(t, "- review the PRs\nthen report\n", got.Prompt, "the file is the prompt, as it is")
}

func TestScheduleTry_RejectsArgumentsThatCannotRun(t *testing.T) {
	t.Parallel()

	emptyFile := filepath.Join(t.TempDir(), "empty.txt")
	require.NoError(t, os.WriteFile(emptyFile, []byte(" \n"), 0o600))
	promptFile := filepath.Join(t.TempDir(), "prompt.txt")
	require.NoError(t, os.WriteFile(promptFile, []byte("go"), 0o600))

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no task", []string{"schedule", "try"}, "accepts between 1 and 2 arg"},
		{"no prompt", []string{"schedule", "try", "task-1"}, "a prompt is required"},
		{"blank prompt", []string{"schedule", "try", "task-1", "  "}, "a prompt is required"},
		{"blank task", []string{"schedule", "try", " ", "go"}, "task is required"},
		{
			"empty prompt file",
			[]string{"schedule", "try", "task-1", "--prompt-file", emptyFile},
			"a prompt is required",
		},
		{
			"missing prompt file",
			[]string{"schedule", "try", "task-1", "--prompt-file", "/nonexistent/p.txt"},
			"read prompt file",
		},
		{"prompt twice", []string{"schedule", "try", "task-1", "go", "--prompt-file", promptFile}, "not both"},
		{"too many arguments", []string{"schedule", "try", "task-1", "go", "again"}, "accepts between 1 and 2 arg"},
		{"unknown mode", []string{"schedule", "try", "task-1", "go", "--mode", "resume"}, `unknown --mode "resume"`},
		{
			"unknown permission",
			[]string{"schedule", "try", "task-1", "go", "--permission", "yolo"},
			`unknown --permission "yolo"`,
		},
		{
			"permission with an underscore",
			[]string{"schedule", "try", "task-1", "go", "--permission", "accept_edits"},
			"accept-edits",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runScheduleCommand(t, tt.args...)

			require.ErrorContains(t, err, tt.want)
			require.Nil(t, got, "nothing is run")
		})
	}
}

func TestScheduleTry_NeedsARuntime(t *testing.T) {
	t.Parallel()

	cmd := newRootCommand(commandRuntime{version: "test-version"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"schedule", "try", "task-1", "go"})

	require.ErrorContains(t, cmd.Execute(), "command not configured")
}

func TestTryTaskPrompt_RunsThePromptInTheTaskNamedByIDOrSlug(t *testing.T) {
	t.Parallel()

	tasks := []*core.Task{
		{ID: "1760000000001", Slug: "billing-retry-flow", DisplayName: "billing retry flow"},
		{ID: "1760000000002", Slug: "docs-cleanup", DisplayName: "docs cleanup"},
		nil,
	}
	for _, ref := range []string{"1760000000002", "docs-cleanup"} {
		t.Run(ref, func(t *testing.T) {
			service := core.NewMockTaskService(t)
			service.EXPECT().ListTasks(mock.Anything).Return(tasks, nil)
			service.EXPECT().RunTaskPrompt(mock.Anything, core.RunTaskPromptInput{
				TaskID:         "1760000000002",
				Prompt:         "triage",
				SessionMode:    core.PromptRunSessionSame,
				PermissionMode: core.TaskPermissionModeBypass,
			}).Return(core.RunTaskPromptResult{Outcome: core.PromptRunLaunched, Detail: "resumed 3f2a…"}, nil)
			var stdout bytes.Buffer

			err := tryTaskPrompt(t.Context(), &stdout, service, scheduleTryRequest{
				Task:           ref,
				Prompt:         "triage",
				SessionMode:    core.PromptRunSessionSame,
				PermissionMode: core.TaskPermissionModeBypass,
			})

			require.NoError(t, err)
			require.Equal(t,
				"Task: docs cleanup (1760000000002)\nOutcome: launched\nDetail: resumed 3f2a…\n",
				stdout.String())
		})
	}
}

func TestTryTaskPrompt_ASkipIsPrintedNotFailed(t *testing.T) {
	t.Parallel()

	service := core.NewMockTaskService(t)
	service.EXPECT().ListTasks(mock.Anything).Return([]*core.Task{{ID: "t1", Slug: "s1", DisplayName: "one"}}, nil)
	service.EXPECT().RunTaskPrompt(mock.Anything, mock.Anything).
		Return(core.RunTaskPromptResult{Outcome: core.PromptRunSkipped, Detail: "skipped: still working"}, nil)
	var stdout bytes.Buffer

	err := tryTaskPrompt(t.Context(), &stdout, service, scheduleTryRequest{Task: "t1", Prompt: "go"})

	require.NoError(t, err)
	require.Contains(t, stdout.String(), "Outcome: skipped\n")
	require.Contains(t, stdout.String(), "Detail: skipped: still working\n")
}

func TestTryTaskPrompt_RefusesATaskThatIsMissingOrAmbiguous(t *testing.T) {
	t.Parallel()

	tasks := []*core.Task{
		{ID: "t1", Slug: "shared-slug"},
		{ID: "t2", Slug: "shared-slug"},
		{ID: "t3", Slug: "unique"},
	}
	tests := []struct {
		ref  string
		want string
	}{
		{"shared-slug", `"shared-slug" matches several tasks (t1, t2)`},
		{"nope", `no task has the ID or slug "nope"`},
		{"uniq", `no task has the ID or slug "uniq"`},
		{"t", `no task has the ID or slug "t"`},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			// The service is never asked to run anything.
			service := core.NewMockTaskService(t)
			service.EXPECT().ListTasks(mock.Anything).Return(tasks, nil)

			err := tryTaskPrompt(t.Context(), &bytes.Buffer{}, service, scheduleTryRequest{Task: tt.ref, Prompt: "go"})

			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestTryTaskPrompt_MatchesTheIDWhenASlugEqualsAnotherTasksID(t *testing.T) {
	t.Parallel()

	service := core.NewMockTaskService(t)
	service.EXPECT().ListTasks(mock.Anything).Return([]*core.Task{
		{ID: "t1", Slug: "t2"},
		{ID: "t2", Slug: "other"},
	}, nil)

	err := tryTaskPrompt(t.Context(), &bytes.Buffer{}, service, scheduleTryRequest{Task: "t2", Prompt: "go"})

	require.ErrorContains(t, err, `"t2" matches several tasks (t1, t2)`)
}

func TestTryTaskPrompt_ReturnsServiceFailures(t *testing.T) {
	t.Parallel()

	t.Run("listing tasks", func(t *testing.T) {
		service := core.NewMockTaskService(t)
		service.EXPECT().ListTasks(mock.Anything).Return(nil, errors.New("daemon unreachable"))

		err := tryTaskPrompt(t.Context(), &bytes.Buffer{}, service, scheduleTryRequest{Task: "t1", Prompt: "go"})

		require.ErrorContains(t, err, "daemon unreachable")
	})
	t.Run("running the prompt", func(t *testing.T) {
		service := core.NewMockTaskService(t)
		service.EXPECT().ListTasks(mock.Anything).Return([]*core.Task{{ID: "t1", Slug: "s1"}}, nil)
		service.EXPECT().RunTaskPrompt(mock.Anything, mock.Anything).
			Return(core.RunTaskPromptResult{}, errors.New("start task session: tmux refused"))
		var stdout bytes.Buffer

		err := tryTaskPrompt(t.Context(), &stdout, service, scheduleTryRequest{Task: "t1", Prompt: "go"})

		require.ErrorContains(t, err, "tmux refused")
		require.Empty(t, stdout.String())
	})
}

func TestScheduleTry_TakesAPromptThatStartsWithADashAfterTheSeparator(t *testing.T) {
	t.Parallel()

	got, err := runScheduleCommand(t, "schedule", "try", "task-1", "--", "- fix everything")

	require.NoError(t, err)
	require.Equal(t, "- fix everything", got.Prompt)
}

func TestScheduleTry_ExplainsHowToGiveAPromptThatStartsWithADash(t *testing.T) {
	t.Parallel()

	got, err := runScheduleCommand(t, "schedule", "try", "task-1", "-starts with a dash")

	require.Nil(t, got)
	require.ErrorContains(t, err, "unknown shorthand flag")
	require.ErrorContains(t, err, `needs "--" before it, or --prompt-file`)
}
