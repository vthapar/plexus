# CLI Package Guidelines

## Design Principles

- The CLI exists for **imperative shortcuts** — simple, common operations.
  For complex specs (availability zones, multiple subnets at once), users
  should use `kubectl apply -f`.
- Do not duplicate what `kubectl` already does well. We dropped `plexus get`
  in favor of `kubectl get and` with printcolumn markers on the CRD.
- `plexus describe` is the exception — it provides a domain-specific view
  that `kubectl describe` cannot match.

## Conventions

- **Output**: Always use `cmd.OutOrStdout()` and `cmd.InOrStdin()`, never
  `os.Stdout` or `os.Stdin` directly. This makes commands testable.
- **Errors**: Return `fmt.Errorf(...)` from `RunE`, never `fmt.Fprintf` +
  `os.Exit`. Let Cobra handle error display.
- **Destructive commands** (`delete`, `delete-subnet`): Must prompt for
  confirmation by default. Add `--yes` / `-y` flag to skip.
- **Validation**: Use the shared helpers in `validate.go` (`validateCIDRs`,
  `validateSubnetType`). Validate client-side before making API calls.
- **Kubeconfig**: Handled by persistent flags (`--kubeconfig`, `--context`)
  on the root command. Use `getClient()` from `client.go` — do not create
  clients directly.
- **Command naming**: Use Kubernetes verbs — `create`, `delete`, `describe`,
  `add-subnet`, `delete-subnet`. Not `remove`, `show`, `list`.
- **Flag style**: Use `--flag-name` (kebab-case). Required flags use
  `cmd.MarkFlagRequired()`. Repeatable flags use `StringSliceVar` or
  `StringArrayVar`.

## Adding a New Command

1. Create `commandname.go` in this package
2. Implement `newCommandNameCommand() *cobra.Command`
3. Register it in `root.go` via `cmd.AddCommand(...)`
4. Add the command to `docs/cli.md`

## File Layout

| File | Purpose |
|------|---------|
| `root.go` | Root command, persistent flags, subcommand registration |
| `client.go` | Shared `getClient()` using kubeconfig/context flags |
| `validate.go` | Shared CIDR and subnet type validation |
| `version.go` | Version subcommand |
| `create.go` | Create an AND with its first subnet |
| `delete.go` | Delete an AND (with confirmation) |
| `describe.go` | Rich AND detail view |
| `addsubnet.go` | Add a subnet to an AND |
| `deletesubnet.go` | Delete a subnet from an AND (with confirmation) |
