# How to contribute

Contributions are welcome. A few things to know first.

## Contributor License Agreement

Contributions to this project must be accompanied by a Contributor License
Agreement. You (or your employer) retain the copyright to your contribution;
the CLA gives us permission to use and redistribute it as part of the project.
See <https://cla.developers.google.com/> for details.

You generally only need to submit a CLA once, so if you have already submitted
one (even for a different Google project), you probably do not need to do it
again.

## Code reviews

All submissions, including submissions by project members, go through a pull
request and get reviewed. See
[GitHub Help](https://help.github.com/articles/about-pull-requests/) if pull
requests are new to you.

## What belongs here, and what belongs upstream

This repo is an integration: it deploys Anthropic Managed Agents Self-Hosted
Sandbox workers on top of
[`agent-substrate/substrate`](https://github.com/agent-substrate/substrate). It
deliberately carries no patches against `agent-substrate/substrate`.

If you hit a gap in the Substrate control plane or runtime, file it there rather
than working around it here.

## Before you open a PR

Please make sure your changes pass all unit tests, linters, and license checks locally:

```bash
make test
make vet
make verify-boilerplate
```

## Running locally

To shell into the container locally with your Anthropic environment variables:

```bash
podman run -it --rm \
  --entrypoint bash \
  -e ANTHROPIC_ENVIRONMENT_ID="$ANTHROPIC_ENVIRONMENT_ID" \
  -e ANTHROPIC_ENVIRONMENT_KEY="$ANTHROPIC_ENVIRONMENT_KEY" \
  gcr.io/$GOOGLE_CLOUD_PROJECT/ate-ant:latest
```

Inside the container, run a worker manually with `ant beta:worker run --work-id "<work-id>" --session-id "<session-id>"`.

## Copyright Headers

Every file containing source code must include copyright and license
information. Our standard headers can be found in `hack/boilerplate/`.

## Community guidelines

This project follows
[Google's Open Source Community Guidelines](https://opensource.google/conduct/).
