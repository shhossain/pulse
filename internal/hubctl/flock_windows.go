package hubctl

import "os"

func flock(*os.File) error { return nil } // pulse hub runs on macOS and Linux
