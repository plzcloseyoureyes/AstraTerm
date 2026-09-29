// Package apitest holds the HTTP-level tests of the importer module: they run a complete in-process AstraTerm server
// (internal/server/servertest, which mounts every module), so they live apart from the unit tests of package
// importer to avoid an import cycle (server imports importer).
package apitest
