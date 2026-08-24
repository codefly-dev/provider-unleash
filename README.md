# provider-unleash

`provider-unleash` is the bounded Codefly provider for the server-side
`codefly.dev/configuration/feature-flags@1` and browser-side
`codefly.dev/configuration/feature-flags-browser@1` contracts.

It resolves the `admin`, `server`, and `edge` semantic endpoints supplied by the
host, manages one declared project/environment/application binding and its
backend/frontend tokens, and projects only the admitted semantic endpoint
references, application/environment identities, provider mode, and opaque
runtime token references. The management credential is used only through the
host broker and is never part of provider input, state, receipts, or output.

Remote deletion defaults to retain. `delete-owned` teardown is accepted only
for resources carrying the exact deterministic binding stamp; imports likewise
require the declared type and exact remote identity. Environment records and API
tokens are retained because the environment is instance-shared and Unleash does
not expose a token deletion identity without exposing credential bytes.

## Binding input

The binding declares `project_id`, `project_name`, `environment_id`,
`environment_type`, `application_id`, and `provider_mode` (`edge`). The host
feeds captured runtime references back with
`server_credential_fingerprint` and `browser_credential_fingerprint`; those safe
fingerprints classify the two opaque references without revealing either token.
Exact adoption additionally supplies `import_resource_type` and
`import_remote_id`. Teardown sets `intent` to `destroy`; all other plans use
`apply`.

## Development

```sh
make test
make vet
make package
```

CI exercises the request catalog, response policy, deterministic lifecycle,
recovery, and hostile host-callback boundaries in process. Live graph dogfood
requires the separately packaged `service-unleash` artifact.
