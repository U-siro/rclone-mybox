// Licensed under Overwatch License Revision 3 with an upstream rclone PR exception.
// See MYBOX-LICENSE at the repository root. Pre-existing third-party rights are preserved.

package mybox_test

import (
	"testing"

	"github.com/rclone/rclone/backend/mybox"
	"github.com/rclone/rclone/fstest/fstests"
)

// TestIntegration runs the standard backend suite when TestMybox is configured.
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{RemoteName: "TestMybox:", NilObject: (*mybox.Object)(nil)})
}
