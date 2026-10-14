# Repository completion rules

Before completing any task in this repository:

1. Write all repository documentation in English. This includes README files, guides, public API documentation, documentation comments, and documentation-oriented examples.
2. Format changed Go code with `gofmt`.
3. Run `golangci-lint run ./...` and fix every reported issue.
4. Run tests with race detection: `go test -race ./...`.
5. Verify test coverage. Changed Go code must have at least 90% line coverage. New and changed behavior must have tests. Do not commit coverage reports.
6. Check that temporary files and unrelated user changes are not included in the commit.
7. Create a commit with a meaningful message and push the current branch to its configured upstream with `git push`.
8. Create an annotated release tag on the final task commit and push that tag to the same upstream remote. Do this for every completed task, including documentation and repository-rule changes, without asking for release approval again. Fetch remote tags first and use the next patch version after the latest release tag unless the user specifies another version. Never move or overwrite an existing release tag.

A task is not complete until every applicable check passes and both the commit and its release tag have been pushed.
