# Glxymesh SDKs

Write [Glxymesh](https://glxymesh.com) function tools in Go, TypeScript or Python. A tool is one
`run` function; Glxymesh builds it, tests it, hosts it on AWS Lambda and serves it to Claude and
other MCP clients from one authenticated endpoint.

| Language | Package | Install | Folder |
|---|---|---|---|
| Go | `glxymesh.com/sdk/glxy` | `go get glxymesh.com/sdk` | [`glxy/`](glxy) |
| TypeScript | `@glxymesh/sdk` | `npm i @glxymesh/sdk` | [`typescript/`](typescript) |
| Python | `glxymesh` | `pip install glxymesh` | [`python/`](python) |

Every SDK gives a tool the same `ctx`: outside requests and database connections by way of the
Glxymesh egress gateway (only the hosts in the tool's `tool.yml`, keys added on the way out so
your code never holds them), log lines, the caller, the deadline, and an error type the model
reads. See the [SDK reference](https://docs.glxymesh.com/reference/sdk/) and the
[quickstart](https://docs.glxymesh.com/quickstart/).

`gxmesh init` and `gxmesh new` set a project up with the right SDK for its language, so most
people never add one by hand.

## Contributing

This repository is published from Glxymesh's main repository, where the SDKs change together
with the platform they talk to. Issues and pull requests are welcome here; accepted changes are
applied upstream and arrive with the next release.

## License

[Apache-2.0](LICENSE)
