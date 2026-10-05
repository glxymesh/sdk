# @glxymesh/sdk

Write [Glxymesh](https://glxymesh.com) function tools in TypeScript.

```ts
import { ToolError, type Ctx } from '@glxymesh/sdk';
import type { Input } from './tool.gen';

export async function run(ctx: Ctx, input: Input) {
  const res = await ctx.fetch(`https://api.github.com/repos/${input.repo}`);
  if (res.status === 404) throw new ToolError(`No repository ${input.repo}.`);
  const repo = await res.json();
  return { stars: repo.stargazers_count };
}
```

Every outside request goes through `ctx.fetch`, and every database connection through
`ctx.connect` or `ctx.forward`, by way of the Glxymesh egress gateway: only the hosts in the
tool's `tool.yml` are reachable, and keys are added there, so your code never holds them.
Throw `ToolError` for a message the model should read. See the
[SDK reference](https://docs.glxymesh.com/reference/sdk/).

License: Apache-2.0
