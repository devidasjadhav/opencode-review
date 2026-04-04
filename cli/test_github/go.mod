module test_github

go 1.25.0

replace github.com/sst/opencode-sdk-go => ../../opencode-sdk-go

require (
	github.com/google/go-github/v67 v67.0.0
	golang.org/x/oauth2 v0.36.0
)

require github.com/google/go-querystring v1.1.0 // indirect
