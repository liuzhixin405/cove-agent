package engine

import (
	"reflect"
	"testing"
	"unsafe"
)

// Every Engine field has a lifetime, and the lifetime says who resets it.
// Fields used to be added without one, and /new, /resume, /cd and
// compaction each reset the subset their author remembered (the review
// throttle survived /new; /resume kept the old interrupted turn). A new
// field fails this test until it is placed:
//
//   - in the embedded conversation struct, when the turn goroutine owns it
//     (cleared as a whole by resetConversationState, nothing else to do);
//   - in conversationManaged, when it belongs to the conversation but is
//     guarded by a lock or needs construction: then resetConversationState
//     (or LoadMessages / setCostBase / enterSession) must reset it;
//   - in projectScoped, when /cd must rebuild it (SetWorkingDir);
//   - in processScoped otherwise.
var (
	conversationManaged = []string{
		"messages", "sessionView", "systemPrompt", "totalTokens", "lastInputTokens", "usageMsgCount",
		"session", "costBase", "fileHistory", "turnFilesChanged", "turnRanGit", "turnChangedFiles", "turnCheckpointed",
		"pendingSteer", "pendingSteerN", "loopDetector", "guardrails", "acceptance",
		"lastReviewMsgCount", "turnsSinceReview", "turnUsedWork", "conversationGen",
		"newMemories", "shownMemories", "repoMapExcerpts", "injectedSkills",
	}
	projectScoped = []string{
		"projCtx", "cpMgr", "verifyGate", "sessionNotes", "repoIndex", "subdirHints", "diskRules",
		"policyLoadErr",
	}
	processScoped = []string{
		"llm", "modelRouter", "registry", "config", "costTracker", "perm", "store", "memStore",
		"skillMgr", "hookMgr", "classifier", "systemOverride", "runtime", "fileMu", "steerMu",
		"cachedToolDefs", "cachedToolDefsVersion", "cachedToolDefsExtra", "toolDefsVersion",
		"compressor", "masker", "safetyChecker", "promptMu", "out",
		"PermissionPrompt", "hooks",
		"IterationLimitPrompt", "OnBackgroundSummary",
		"OnSteerConsumed", "bg", "bgPending", "reviewBg", "extractSaved", "lastSaveErr", "bgMu",
		"dreamSeen", "lastDreamNeeded", "turnTimeUnit", "rateLimits", "extractRunner", "backgroundModel",
		"autoLearnOff", "dreamRunner", "fastOutcomes", "recordingEnabled", "recordingDir", "recordingSeq", "acceptanceMu",
		"recordingReady", "recordingMu", "replayEnabled", "replayDir", "replayResponses", "replayIndex",
		"actMu", "acts", "actSeq", "provRef", "collectContext", "refreshGit", "costNoticeFor",
		"fileDiffs", "diffMu", "contextTokens", "turnModelSnap", "repoMapMu", "reviewRunning", "nonInteractive", "skillMu", "requestOverhead", "smallWindowWarned",
	}
)

func TestEveryEngineFieldHasALifetime(t *testing.T) {
	placed := map[string]string{"conversation": "embedded"}
	for kind, names := range map[string][]string{
		"conversationManaged": conversationManaged, "projectScoped": projectScoped, "processScoped": processScoped,
	} {
		for _, n := range names {
			if prev, dup := placed[n]; dup {
				t.Errorf("field %s is listed as both %s and %s", n, prev, kind)
			}
			placed[n] = kind
		}
	}
	ty := reflect.TypeOf(Engine{})
	fields := map[string]bool{}
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		fields[name] = true
		if _, ok := placed[name]; !ok {
			t.Errorf("Engine.%s has no lifetime: add it to the conversation struct or to one of the lists in state_ownership_test.go (and reset it where that list says)", name)
		}
	}
	for n := range placed {
		if !fields[n] {
			t.Errorf("state_ownership_test.go lists %s, which is not an Engine field", n)
		}
	}
}

// resetConversationState clears every field of the conversation struct.
func TestResetClearsTheWholeConversation(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	v := reflect.ValueOf(&eng.conversation).Elem()
	for i := 0; i < v.NumField(); i++ {
		// Unexported fields are settable through their address.
		f := reflect.NewAt(v.Field(i).Type(), unsafe.Pointer(v.Field(i).UnsafeAddr())).Elem()
		switch f.Kind() {
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int:
			f.SetInt(7)
		case reflect.String:
			f.SetString("x")
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		case reflect.Map:
			f.Set(reflect.MakeMap(f.Type()))
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		default:
			t.Fatalf("conversation.%s has kind %s the test cannot fill", v.Type().Field(i).Name, f.Kind())
		}
	}
	eng.resetConversationState()
	if !reflect.ValueOf(eng.conversation).IsZero() {
		t.Fatalf("conversation not cleared: %+v", eng.conversation)
	}
}
