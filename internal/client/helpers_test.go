package client

import (
	"errors"

	"pulse/internal/model"
)

func modelMeta() model.RunMeta       { return model.RunMeta{Source: "run"} }
func errorsAs(err error, t any) bool { return errors.As(err, t) }
