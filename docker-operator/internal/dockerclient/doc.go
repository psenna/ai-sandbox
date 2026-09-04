// Package dockerclient is a narrow interface over the moby SDK covering only
// the operations the agent lifecycle needs: volumes, networks, containers,
// exec and the event stream. Narrowing it here keeps the lifecycle code
// unit-testable against a fake instead of a live daemon.
//
// This is the ONLY package in docker-operator permitted to import the moby
// SDK. Every signature deals in this package's own types, so a consumer that
// depends on Client cannot reach a moby type even by accident.
//
// The SDK is github.com/moby/moby/client (types in github.com/moby/moby/api),
// the standalone client module moby split out of github.com/docker/docker.
// The old docker/docker module is not usable here: it is the whole engine, so
// DependaProxy's CVE gate rejects every published version over daemon-side
// advisories that do not apply to a client.
//
// A fake implementation lives in the dockerclienttest subpackage.
package dockerclient
