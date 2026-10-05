# glxymesh

Write [Glxymesh](https://glxymesh.com) function tools in Python.

```python
from glxymesh import Ctx, ToolError
from .tool_gen import Input


def run(ctx: Ctx, input: Input) -> dict:
    r = ctx.fetch("https://api.github.com/repos/" + input["repo"]).raise_for_status()
    return {"stars": r.json()["stargazers_count"]}
```

Every outside request goes through `ctx.fetch`, by way of the Glxymesh egress
gateway: the hosts in tool.yml are the only ones it reaches, and keys are added
there, so your code never holds them. Raise `ToolError` for a message the model
should read. See the [SDK reference](https://docs.glxymesh.com/reference/sdk/).
