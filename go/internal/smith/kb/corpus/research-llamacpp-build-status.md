ref: research:llamacpp-build-status
doc: research
slug: llamacpp-build-status
title: Checking your llama.cpp build status
category: build
source: docs/pitfalls.md

Whichever llama.cpp build (or fork) you run, it's worth periodically checking two things: is
your build's own commit behind that fork's upstream HEAD, and has a fork's *recorded* build SHA
drifted from what upstream actually has at that commit (i.e. did the fork's maintainer rebase or
update without you rebuilding against it)? Neither failure mode is visible from the outside -
the server keeps running fine either way - so nothing surfaces it unless something explicitly
tracks build provenance and diffs it against upstream.

If you're running a fork specifically for one architecture's device-code support (e.g. a
flash-attention kernel patch for hardware upstream doesn't yet support natively), also watch
that fork's upstream for the same support landing in mainline - once it does, the fork-specific
build may no longer be necessary, and mainline gets you future improvements the fork won't.

Rebuilding is the standard remedy once a drift or a meaningful upstream change is confirmed; see
the build-refresh runbook for the mechanics.
