package executor

import (
	"io/fs"
	"os"
)

// copyOwner does nothing on Windows, which is used for development only.
func copyOwner(*os.File, fs.FileInfo) {}
