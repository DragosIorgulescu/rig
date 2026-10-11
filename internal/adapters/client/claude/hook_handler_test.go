package claude

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/BaronBonet/rig/internal/core"
)

func fixedNow() time.Time {
	return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
}

func TestDecodeHookEventInput_DecodesSessionStartPayload(t *testing.T) {
	body := []byte(`{
		"session_id": "sess-1",
		"transcript_path": "/tmp/transcript.jsonl",
		"cwd": "/tmp/repo-task",
		"hook_event_name": "SessionStart",
		"source": "startup"
	}`)

	input := DecodeHookEventInput(fixedNow, "SessionStart", body)

	require.Equal(t, core.ProviderClaude, input.Provider)
	require.Equal(t, "SessionStart", input.EventName)
	require.Equal(t, "sess-1", input.SessionID)
	require.Equal(t, "/tmp/transcript.jsonl", input.TranscriptPath)
	require.Equal(t, "/tmp/repo-task", input.Cwd)
	require.Equal(t, "startup", input.StartSource)
	require.Empty(t, input.TaskID)
	require.Equal(t, fixedNow(), input.OccurredAt)
}

func TestDecodeHookEventInput_DecodesPromptAndToolPayloads(t *testing.T) {
	prompt := DecodeHookEventInput(fixedNow, "", []byte(`{
		"session_id": "sess-1",
		"hook_event_name": "UserPromptSubmit",
		"prompt": "fix the billing retry flow"
	}`))
	require.Equal(t, "UserPromptSubmit", prompt.EventName)
	require.Equal(t, "fix the billing retry flow", prompt.PromptText)

	tool := DecodeHookEventInput(fixedNow, "PostToolUse", []byte(`{
		"session_id": "sess-1",
		"tool_input": {"command": "go test ./..."},
		"tool_response": "ok"
	}`))
	require.Equal(t, "PostToolUse", tool.EventName)
	require.Equal(t, "go test ./...", tool.CommandText)
	require.Equal(t, "ok", tool.CommandResultText)
}

func TestDecodeHookEventInput_CountsInFlightBackgroundTasksOnStop(t *testing.T) {
	// Shape captured from Claude Code 2.1.286. A Monitor tool watch is
	// reported as a "shell" task.
	input := DecodeHookEventInput(fixedNow, "Stop", []byte(`{
		"session_id": "sess-1",
		"hook_event_name": "Stop",
		"stop_hook_active": false,
		"background_tasks": [
			{"id": "bc4mtriyg", "type": "shell", "status": "running",
			 "description": "QA import run", "command": "heroku logs --tail"},
			{"id": "a1", "type": "subagent", "status": "running", "agent_type": "Explore"},
			{"id": "a2", "type": "subagent", "status": "pending", "agent_type": "general-purpose"},
			{"id": "m1", "type": "monitor", "status": "running", "server": "ci"},
			{"id": "w1", "type": "workflow", "status": "running", "name": "review"},
			{"id": "t1", "type": "teammate", "status": "running"},
			{"id": "d1", "type": "dream", "status": "running"},
			{"id": "a3", "type": "subagent", "status": "completed"}
		],
		"session_crons": []
	}`))

	require.Equal(t, core.TaskBackgroundWork{
		Subagents: 2,
		Shells:    1,
		Monitors:  1,
		Workflows: 1,
		Other:     1,
	}, input.BackgroundWork)
}

func TestDecodeHookEventInput_DecodesNotificationTypeAndSubagentID(t *testing.T) {
	input := DecodeHookEventInput(fixedNow, "Notification", []byte(`{
		"session_id": "sess-1",
		"agent_id": "agent-7",
		"agent_type": "Explore",
		"notification_type": "permission_prompt",
		"message": "Claude needs your permission to use Bash"
	}`))

	require.Equal(t, "permission_prompt", input.NotificationType)
	require.Equal(t, "agent-7", input.AgentID)
	require.Equal(t, "Explore", input.AgentType)
}

func TestDecodeHookEventInput_ToleratesMalformedPayload(t *testing.T) {
	input := DecodeHookEventInput(fixedNow, "", []byte(`not-json`))

	require.Equal(t, core.ProviderClaude, input.Provider)
	require.Equal(t, "unknown", input.EventName)
}

func TestHookEventToTaskStatus_MapsClaudeEventsToPhases(t *testing.T) {
	repo := &repository{binary: "claude"}

	cases := []struct {
		event string
		phase core.TaskStatusPhase
	}{
		{"SessionStart", core.TaskStatusPhaseStarting},
		{"UserPromptSubmit", core.TaskStatusPhaseWorking},
		{"PreToolUse", core.TaskStatusPhaseWorking},
		{"PostToolUse", core.TaskStatusPhaseWorking},
		{"Stop", core.TaskStatusPhaseWaitingForInput},
		// An API error ends the turn with StopFailure instead of Stop.
		{"StopFailure", core.TaskStatusPhaseWaitingForInput},
		{"Notification", core.TaskStatusPhaseWaitingForInput},
		// Exiting, or killing the session's window, ends the process.
		{"SessionEnd", core.TaskStatusPhaseStopped},
	}
	for _, tc := range cases {
		update, err := repo.HookEventToTaskStatus(core.HookEventInput{
			TaskID:     "task-1",
			EventName:  tc.event,
			OccurredAt: fixedNow(),
		})
		require.NoError(t, err, tc.event)
		require.NotNil(t, update, tc.event)
		require.Equal(t, tc.phase, update.Phase, tc.event)
		require.Equal(t, core.ProviderClaude, update.Provider, tc.event)
	}
}

func TestHookEventToTaskStatus_AResumedSessionWaitsForTheUser(t *testing.T) {
	repo := &repository{binary: "claude"}

	for source, phase := range map[string]core.TaskStatusPhase{
		"resume":  core.TaskStatusPhaseWaitingForInput,
		"startup": core.TaskStatusPhaseStarting,
	} {
		update, err := repo.HookEventToTaskStatus(core.HookEventInput{
			TaskID: "task-1", EventName: "SessionStart", StartSource: source, OccurredAt: fixedNow(),
		})
		require.NoError(t, err, source)
		require.Equal(t, phase, update.Phase, source)
	}
}

func TestHookEventToTaskStatus_StopWithBackgroundWorkReportsWorkingInBackground(t *testing.T) {
	repo := &repository{binary: "claude"}
	work := core.TaskBackgroundWork{Subagents: 1, Shells: 1}

	update, err := repo.HookEventToTaskStatus(core.HookEventInput{
		TaskID:         "task-1",
		EventName:      "Stop",
		OccurredAt:     fixedNow(),
		BackgroundWork: work,
	})

	require.NoError(t, err)
	require.NotNil(t, update)
	require.Equal(t, core.TaskStatusPhaseWorkingInBackground, update.Phase)
	require.Equal(t, work, update.BackgroundWork)
	require.Equal(t, "Stop", update.RawEventName)
}

func TestHookEventToTaskStatus_StopWithOnlyShellsAndMonitorsNeedsInput(t *testing.T) {
	// A dev server or an artifact watch runs on its own; ending, or being
	// killed, it only queues a notice for the next prompt, so no Stop follows.
	repo := &repository{binary: "claude"}
	work := core.TaskBackgroundWork{Shells: 1, Monitors: 7, Other: 1}

	update, err := repo.HookEventToTaskStatus(core.HookEventInput{
		TaskID:         "task-1",
		EventName:      "Stop",
		OccurredAt:     fixedNow(),
		BackgroundWork: work,
	})

	require.NoError(t, err)
	require.Equal(t, core.TaskStatusPhaseWaitingForInput, update.Phase)
	require.Equal(t, work, update.BackgroundWork, "the counts are kept to show")
}

func TestHookEventToTaskStatus_StopWithoutBackgroundWorkNeedsInput(t *testing.T) {
	repo := &repository{binary: "claude"}

	update, err := repo.HookEventToTaskStatus(core.HookEventInput{
		TaskID:     "task-1",
		EventName:  "Stop",
		OccurredAt: fixedNow(),
	})

	require.NoError(t, err)
	require.NotNil(t, update)
	require.Equal(t, core.TaskStatusPhaseWaitingForInput, update.Phase)
	require.True(t, update.BackgroundWork.IsZero())
}

func TestHookEventToTaskStatus_OnlyNotificationsAskingForInputDriveStatus(t *testing.T) {
	repo := &repository{binary: "claude"}

	for _, notificationType := range []string{
		"permission_prompt",
		"worker_permission_prompt",
		"elicitation_dialog",
		"elicitation_url_dialog",
		"agent_needs_input",
	} {
		update, err := repo.HookEventToTaskStatus(core.HookEventInput{
			TaskID:           "task-1",
			EventName:        "Notification",
			NotificationType: notificationType,
		})
		require.NoError(t, err, notificationType)
		require.NotNil(t, update, notificationType)
		require.Equal(t, core.TaskStatusPhaseWaitingForInput, update.Phase, notificationType)
	}

	// idle_prompt fires a minute after every turn end, including while
	// background work is still running.
	for _, notificationType := range []string{"idle_prompt", "auth_success", "agent_completed"} {
		update, err := repo.HookEventToTaskStatus(core.HookEventInput{
			TaskID:           "task-1",
			EventName:        "Notification",
			NotificationType: notificationType,
		})
		require.NoError(t, err, notificationType)
		require.Nil(t, update, notificationType)
	}
}

func TestHookEventToTaskStatus_SubagentHooksDriveStatusOnlyWhenAskingForInput(t *testing.T) {
	repo := &repository{binary: "claude"}

	for _, event := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"} {
		update, err := repo.HookEventToTaskStatus(core.HookEventInput{
			TaskID:    "task-1",
			EventName: event,
			AgentID:   "agent-7",
		})
		require.NoError(t, err, event)
		require.Nil(t, update, event)
	}

	update, err := repo.HookEventToTaskStatus(core.HookEventInput{
		TaskID:           "task-1",
		EventName:        "Notification",
		AgentID:          "agent-7",
		NotificationType: "permission_prompt",
	})
	require.NoError(t, err)
	require.NotNil(t, update)
	require.Equal(t, core.TaskStatusPhaseWaitingForInput, update.Phase)
}

func TestHookEventToTaskStatus_IgnoresUnmappedEventsAndMissingTask(t *testing.T) {
	repo := &repository{binary: "claude"}

	update, err := repo.HookEventToTaskStatus(core.HookEventInput{
		TaskID:    "task-1",
		EventName: "SubagentStop",
	})
	require.NoError(t, err)
	require.Nil(t, update)

	_, err = repo.HookEventToTaskStatus(core.HookEventInput{EventName: "Stop"})
	require.ErrorIs(t, err, core.ErrUnmanagedHookEvent)
}

func TestDecodeHookEventInput_StopFailureCarriesWhyTheTurnEnded(t *testing.T) {
	tests := map[string]struct {
		body string
		want string
	}{
		"the error as the conversation showed it": {
			body: `{"hook_event_name": "StopFailure", "error": "rate_limit", "error_details": "429 Too Many Requests",
				"last_assistant_message": "API Error: Rate limit reached"}`,
			want: "API Error: Rate limit reached",
		},
		"the details of the error": {
			body: `{"hook_event_name": "StopFailure", "error": "rate_limit", "error_details": "429 Too Many Requests"}`,
			want: "429 Too Many Requests",
		},
		"details that are not text": {
			body: `{"hook_event_name": "StopFailure", "error": "server_error", "error_details": {"status": 500}}`,
			want: `{"status": 500}`,
		},
		"the kind of error": {
			body: `{"hook_event_name": "StopFailure", "error": "authentication_failed"}`,
			want: "authentication_failed",
		},
		"nothing": {
			body: `{"hook_event_name": "StopFailure"}`,
			want: "",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			input := DecodeHookEventInput(fixedNow, "StopFailure", []byte(tt.body))

			require.Equal(t, core.HookEventStopFailure, input.EventName)
			require.Equal(t, tt.want, input.ErrorText)
		})
	}
}

func TestDecodeHookEventInput_OtherEventsCarryNoErrorText(t *testing.T) {
	input := DecodeHookEventInput(fixedNow, "Stop", []byte(`{
		"hook_event_name": "Stop",
		"error": "rate_limit",
		"last_assistant_message": "All done"
	}`))

	require.Empty(t, input.ErrorText)
	require.Empty(t, input.LastAssistantMessage, "a Stop's message is not recorded as activity by this change")
}
