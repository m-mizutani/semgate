package semgate

import "github.com/m-mizutani/goerr/v2"

var (
	errInvalidConfig = goerr.New("invalid config")
	errAnswerMissing = goerr.New("answer missing in response")
	errInvalidAnswer = goerr.New("invalid answer")
)
