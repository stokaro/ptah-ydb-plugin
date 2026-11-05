// Package yqlschema reads the YQL schema documents of a schema.read-sql request
// without a database session. It is a port of the host's YQL schema reader:
// the documents of one load are tokenized by internal/yql and read in order as
// one script, and a load the host refuses is refused with the host's words, at
// the position the host names.
//
// [Reply] answers the request with the tables, views, principals, grants and
// native objects the load declares, what each document declares, the coverage
// the load claims and a trace to the statement of every object. internal/backend
// serves the request through it.
package yqlschema
