package state

import "testing"

func TestMirrorMarkerNotSpoofableByVisibleText(t *testing.T) {
	real := "**via Forgejo:** I'm just quoting the mirror format, not a real mirrored comment"
	if IsMirroredComment(real) {
		t.Fatal("a genuine comment that merely starts with the old visible prefix must not be treated as mirrored")
	}

	marked := MarkMirrored("**via Forgejo:**\n\nsome mirrored body")
	if !IsMirroredComment(marked) {
		t.Fatal("a body carrying the invisible marker must be recognized as mirrored")
	}
}
