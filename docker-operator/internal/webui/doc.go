// Package webui embeds the static frontend (docker-operator/web) using Go's
// embed directive and serves it, so the shipped binary needs no sidecar files.
//
// Scaffold only (issue #61); the embed directive and the file server land
// in a later task, together with the web/ assets themselves.
package webui
