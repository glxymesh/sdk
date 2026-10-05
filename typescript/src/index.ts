// @glxymesh/sdk is what a Node function tool is written against. A tool
// is a plain function, export async function run(ctx, input); only the
// gateway speaks MCP. Its code reaches the internet through ctx.fetch, which
// goes by the egress gateway: the tool's allow-list holds there, and its keys
// are added there, so the code never holds them.

// Who the call is for, as code may see them: never an email or a token.
export interface Caller {
  readonly org: string;
  readonly project: string;
  readonly userId: string;
  // The MCP client the person allowed, such as Claude or Cursor.
  readonly clientId: string;
}

// Lines ride back with the call's result and show beside the call in the
// console; tool Lambdas write nothing to CloudWatch.
export interface Logger {
  info(message: string): void;
  warn(message: string): void;
  error(message: string): void;
}

export interface Ctx {
  readonly caller: Caller;
  readonly log: Logger;
  // Aborts at the call's deadline. Pass it to anything that can stop early.
  readonly signal: AbortSignal;
  // Milliseconds left before the deadline.
  remaining(): number;
  // fetch as the platform knows it, by way of the egress gateway: a host
  // tool.yml doesn't allow throws an EgressError that says what to add.
  fetch(input: string | URL | Request, init?: RequestInit): Promise<Response>;
  // A TCP connection, for a database or another protocol that isn't HTTPS,
  // by way of the egress gateway: address is host:port as the tool's
  // network.connect lists it, or the server a connection string names, for
  // code that speaks the protocol itself. It ends with the call.
  connect(address: string): Promise<import('node:net').Socket>;
  // Listens on 127.0.0.1 and carries each connection made there to address,
  // for a driver that opens its own sockets, like pg: give it the host and
  // port this returns. It closes when the call ends.
  forward(address: string): Promise<{ host: string; port: number }>;
  // A passed-in key's value for this call: a key tool.yml binds with
  // mode: pass-in, which a person approved before the revision went live.
  secretValue(name: string): Promise<string>;
}

// Ends a call with a message the model reads and can act on. Anything else
// thrown is treated as a bug: the model gets a fixed sentence and the owner
// the log.
export class ToolError extends Error {
  override name = 'ToolError';
}

// A request the egress gateway refused or couldn't complete. Its message
// tells the tool's author what to add to tool.yml.
export class EgressError extends Error {
  override name = 'EgressError';
  constructor(
    message: string,
    readonly decision: string,
  ) {
    super(message);
  }
}

// Stands in for a key where no header or query binding fits, such as a JSON
// body. The egress gateway swaps in the value, and only on the hosts the key
// is bound to; anywhere else the request is refused.
export function secret(name: string): string {
  return `{{secret:${name}}}`;
}
