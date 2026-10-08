package remote

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCredentialProtectedACL(t *testing.T) {
	_, path, err := createCredential(filepath.Join(t.TempDir(), "token"))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	expected := "D:P(A;;FA;;;" + user.User.Sid.String() + ")"
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("credential ACL is not protected: %v %v", control, err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("credential ACL must contain exactly one ACE: %v %v", dacl, err)
	}
	reference, err := windows.SecurityDescriptorFromString(expected)
	if err != nil {
		t.Fatal(err)
	}
	referenceDACL, _, err := reference.DACL()
	if err != nil {
		t.Fatal(err)
	}
	var actualACE, referenceACE *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &actualACE); err != nil {
		t.Fatal(err)
	}
	if err := windows.GetAce(referenceDACL, 0, &referenceACE); err != nil {
		t.Fatal(err)
	}
	actualSID := (*windows.SID)(unsafe.Pointer(&actualACE.SidStart))
	if actualACE.Header != referenceACE.Header || actualACE.Mask != referenceACE.Mask || actualSID.String() != user.User.Sid.String() {
		t.Fatalf("credential ACL is not current-user-only full access: got=%q want=%q", descriptor.String(), expected)
	}
}
