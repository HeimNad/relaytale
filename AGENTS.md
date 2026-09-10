# Project workflow

## Git checkpoints

- Use Git for each development phase. The user has authorized local repository initialization and phase checkpoint commits.
- At the end of each completed phase, update the README and roadmap, run the relevant checks, inspect the staged diff, and commit the phase's changes with a clear message.
- Do not commit unrelated or unfinished user changes. Keep incomplete work visible; do not reset, clean, or discard it to make a checkpoint.
- Tag validated phase milestones with annotated tags such as `phase-1`. Never move an existing milestone tag without explicit instruction.
- Do not commit secrets, local environment files, private keys, generated certificates, database data, EML archives, or build artifacts.
- Use feature branches for substantial subsequent phases. Do not rewrite published history or push to a remote unless the user requests it.
- Report the commit and checks performed when completing a phase. If checks fail, fix them or clearly record the limitation rather than describing the phase as validated.

## CodeGraph

Prefer the configured CodeGraph tools for structural code exploration. Start architecture context with `codegraph_context`, flow tracing with `codegraph_trace`, and inspect related bodies with one `codegraph_explore` call. Use native search for literal text or already identified files. Trust fresh results; read only files identified as stale. Do not delegate structural exploration. If CodeGraph is uninitialized, ask before running `codegraph init -i`.
