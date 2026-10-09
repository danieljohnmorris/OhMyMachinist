# Factory delivery example

This example shows a generic, non-merging delivery workflow. Configure absolute
paths and commands in Machinist. The script has no provider-specific URL, host,
or credential built in.

Required environment for a real run:

```sh
export PLANE_URL=https://plane.example.com
export PLANE_WORKSPACE=your-workspace
export PLANE_API_KEY=
export LINEAR_API_KEY=
```

The workflow can be exercised without side effects:

```sh
./examples/factory/delivery.sh dry-run
```

In normal operation, Machinist passes the issue key and prompt, runs code and
review prompts separately, then reports metadata back to the source issue. The
script only creates a branch, pushes it, opens a draft PR, captures optional
screenshots, and comments. It has no merge or deploy command.
