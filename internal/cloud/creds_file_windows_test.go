package cloud

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func assertRestrictedCredentials(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("read credential ACL: %v", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("read credential DACL: %v", err)
	}
	if dacl == nil {
		t.Fatal("credential DACL is absent")
	}
	if dacl.AceCount != 1 {
		t.Fatalf("credential DACL has %d entries, want one current-user entry", dacl.AceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatalf("read credential ACE: %v", err)
	}
	currentUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("read current user SID: %v", err)
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSID.Equals(currentUser.User.Sid) {
		t.Fatalf("credential ACL belongs to %s, want current user %s", aceSID, currentUser.User.Sid)
	}
	const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff
	if ace.Mask&fileAllAccess != fileAllAccess {
		t.Fatalf("credential ACL mask = %#x, want FILE_ALL_ACCESS", ace.Mask)
	}
}
