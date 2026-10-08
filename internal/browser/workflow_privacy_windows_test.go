package browser

import (
	"context"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWorkflowWindowsPrivateEvidence(t *testing.T) {
	report, _ := New(DefaultConfig()).Run(context.Background(), Workflow{}, RunOptions{EvidenceDir: t.TempDir()})
	if report.EvidenceDir == "" {
		t.Fatal("private evidence directory not created")
	}
	for _, path := range []string{report.EvidenceDir, filepath.Join(report.EvidenceDir, "report.json")} {
		descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := descriptor.DACL()
		if err != nil || dacl == nil || dacl.AceCount != 1 {
			t.Fatalf("evidence must have one current-user ACL: %v %v", dacl, err)
		}
	}
	descriptor, err := windows.GetNamedSecurityInfo(report.EvidenceDir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("unprotected directory ACL: %v %v", control, err)
	}
}
