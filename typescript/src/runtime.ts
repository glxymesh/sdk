// The project Lambda's side of a call, which the generated router hands its
// tools to; a tool author never imports it. It speaks the JSON forms of the
// glxymesh/tool/v1 messages, so the bundle carries no protobuf runtime.
import type {
  FetchRequestJson,
  FetchResponseJson,
  InvokeEventJson,
  InvokeResultJson,
  LogLineJson,
} from './gen/tool_pb.js';
import { type Caller, type Ctx, EgressError, type Logger, ToolError } from './index.js';
import { createServer, connect as netConnect, type Server, type Socket } from 'node:net';

export interface Tool {
  run(ctx: Ctx, input: never): unknown;
  // tool.yml's defaults, for inputs a call leaves out.
  defaults: Record<string, unknown>;
}

// Past 64 KB a call's log lines are dropped and the result says so.
const maxLogBytes = 64 << 10;
// The egress gateway's own cap; a larger body would be refused there anyway.
const maxBodyBytes = 4 << 20;

class Lines implements Logger {
  lines: LogLineJson[] = [];
  size = 0;
  truncated = false;
  info(message: string) {
    this.add('info', message);
  }
  warn(message: string) {
    this.add('warn', message);
  }
  error(message: string) {
    this.add('error', message);
  }
  private add(level: string, message: string) {
    const text = String(message);
    if (this.size + text.length > maxLogBytes) {
      this.truncated = true;
      return;
    }
    this.size += text.length;
    this.lines.push({ at: new Date().toISOString(), level, message: text });
  }
}

// handler is the Lambda handler the router exports. Every call is answered
// with a result, crashes included, so Lambda never reports a failure of its
// own for a tool's bug.
export function handler(tools: Record<string, Tool>, egressURL = process.env['GLXY_EGRESS_URL'] ?? '') {
  return async (event: InvokeEventJson): Promise<InvokeResultJson> => {
    const log = new Lines();
    const tool = tools[event.tool ?? ''];
    if (!tool) return crash(`this build has no tool named ${event.tool}`, log);
    const deadline = event.deadline ? Date.parse(event.deadline) : Date.now() + 10_000;
    // No call outlives Lambda's 15 minutes, whatever the event says.
    const signal = AbortSignal.timeout(Math.min(Math.max(1, deadline - Date.now()), 900_000));
    const c = event.caller ?? {};
    const caller: Caller = { org: c.org ?? '', project: c.project ?? '', userId: c.userId ?? '', clientId: c.clientId ?? '' };
    const token = event.egressToken ?? '';
    const open: (Socket | Server)[] = [];
    const ctx: Ctx = {
      caller,
      log,
      signal,
      remaining: () => Math.max(0, deadline - Date.now()),
      fetch: (input, init) => egressFetch(egressURL, token, signal, input, init),
      connect: async (address) => {
        const s = await egressConnect(egressURL, token, address);
        open.push(s);
        return s;
      },
      forward: async (address) => {
        const server = createServer((local) => {
          egressConnect(egressURL, token, address).then(
            (remote) => {
              open.push(remote);
              local.pipe(remote).pipe(local);
              local.on('error', () => remote.destroy());
              remote.on('error', () => local.destroy());
            },
            (err) => {
              log.error(`forward to ${address}: ${err instanceof Error ? err.message : String(err)}`);
              local.destroy();
            },
          );
        });
        open.push(server);
        await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
        const a = server.address() as { port: number };
        return { host: '127.0.0.1', port: a.port };
      },
      secretValue: (name) => egressSecret(egressURL, token, signal, name),
    };
    const ending = () => {
      for (const o of open) 'destroy' in o ? o.destroy() : o.close();
    };
    let out: InvokeResultJson;
    try {
      const given = event.argumentsJson ? JSON.parse(event.argumentsJson) : {};
      const input = { ...tool.defaults, ...(given ?? {}) };
      const result = await tool.run(ctx, input as never);
      const json = JSON.stringify(result === undefined ? null : result);
      if (json === undefined) {
        ending();
        return crash("the result isn't JSON", log);
      }
      out = { resultJson: json };
    } catch (err) {
      if (err instanceof ToolError) {
        out = { errorKind: 'ERROR_KIND_TOOL', errorMessage: err.message };
      } else {
        log.error(err instanceof Error ? (err.stack ?? err.message) : String(err));
        ending();
        return crash(err instanceof Error ? err.message : String(err), log);
      }
    }
    ending();
    return { ...out, logs: log.lines, logsTruncated: log.truncated };
  };
}

// egressConnect upgrades a connection to the egress gateway's /connect into
// a TCP connection to address; what follows the gateway's 101 is the
// upstream's bytes.
function egressConnect(egressURL: string, token: string, address: string): Promise<Socket> {
  if (!egressURL) return Promise.reject(new EgressError('egress: no egress gateway is configured for this call', 'EGRESS_DECISION_FAILED'));
  const u = new URL(egressURL);
  return new Promise((resolve, reject) => {
    const s = netConnect(Number(u.port || 80), u.hostname, () => {
      s.write(
        `GET /connect HTTP/1.1\r\nHost: ${u.host}\r\nConnection: Upgrade\r\nUpgrade: glxymesh-tcp\r\n` +
          `Authorization: Bearer ${token}\r\nX-Glxymesh-Connect: ${address}\r\n\r\n`,
      );
    });
    let head = Buffer.alloc(0);
    const onData = (chunk: Buffer) => {
      head = Buffer.concat([head, chunk]);
      const end = head.indexOf('\r\n\r\n');
      if (end < 0) {
        if (head.length > 16 << 10) fail(new Error('egress: the egress gateway sent no answer'));
        return;
      }
      s.off('data', onData);
      s.off('error', fail);
      const status = Number(head.subarray(0, end).toString().split(' ')[1]);
      const rest = head.subarray(end + 4);
      if (status !== 101) {
        let why = rest.toString().trim();
        try {
          why = (JSON.parse(why) as { error?: string }).error ?? why;
        } catch {
          // a plain-text refusal
        }
        s.destroy();
        reject(new EgressError(`egress: ${why || `the egress gateway answered ${status}`}`, 'EGRESS_DECISION_HOST_NOT_ALLOWED'));
        return;
      }
      if (rest.length) s.unshift(rest);
      resolve(s);
    };
    const fail = (err: Error) => {
      s.destroy();
      reject(err);
    };
    s.on('data', onData);
    s.on('error', fail);
  });
}

async function egressSecret(egressURL: string, token: string, signal: AbortSignal, name: string): Promise<string> {
  if (!egressURL) throw new EgressError('egress: no egress gateway is configured for this call', 'EGRESS_DECISION_FAILED');
  const res = await fetch(`${egressURL}/secret/${encodeURIComponent(name)}`, { headers: { Authorization: `Bearer ${token}` }, signal });
  const body = (await res.json().catch(() => ({}))) as { value?: string; error?: string };
  if (res.status !== 200) throw new ToolError(body.error ?? `the egress gateway answered ${res.status}`);
  return body.value ?? '';
}

function crash(message: string, log: Lines): InvokeResultJson {
  return { errorKind: 'ERROR_KIND_CRASH', errorMessage: message, logs: log.lines, logsTruncated: log.truncated };
}

// Every outside request goes to the egress gateway's /fetch with the call's
// token: a tool Lambda's network has no other route, and its DNS answers
// nothing else.
async function egressFetch(
  egressURL: string,
  token: string,
  deadline: AbortSignal,
  input: string | URL | Request,
  init?: RequestInit,
): Promise<Response> {
  if (!egressURL) throw new EgressError('egress: no egress gateway is configured for this call', 'EGRESS_DECISION_FAILED');
  const req = new Request(input, init);
  const body = req.body ? Buffer.from(await req.arrayBuffer()) : undefined;
  if (body && body.length > maxBodyBytes) throw new EgressError('egress: request body over 4 MB', 'EGRESS_DECISION_TOO_LARGE');
  const fr: FetchRequestJson = {
    method: req.method,
    url: req.url,
    headers: [...req.headers].map(([name, value]) => ({ name, value })),
    ...(body ? { body: body.toString('base64') } : {}),
  };
  const res = await fetch(`${egressURL}/fetch`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify(fr),
    signal: init?.signal ? AbortSignal.any([init.signal, deadline]) : deadline,
  });
  if (res.status !== 200) throw new Error(`egress: the egress gateway answered ${res.status}`);
  const got = (await res.json()) as FetchResponseJson;
  if (!got.status) throw new EgressError(`egress: ${got.error ?? 'refused'}`, got.decision ?? 'EGRESS_DECISION_FAILED');
  const headers = new Headers();
  for (const h of got.headers ?? []) headers.append(h.name ?? '', h.value ?? '');
  const empty = got.status === 204 || got.status === 205 || got.status === 304;
  return new Response(empty || !got.body ? null : Buffer.from(got.body, 'base64'), { status: got.status, headers });
}

// serve takes calls from a Lambda Runtime API until the process ends. In
// Lambda the Node runtime does this itself; gxmesh dev runs the bundle with
// GLXY_RUNTIME_API pointing at its own stand-in.
export async function serve(handle: (event: InvokeEventJson) => Promise<InvokeResultJson>, api: string): Promise<never> {
  const base = `http://${api}/2018-06-01/runtime/invocation/`;
  for (;;) {
    const next = await fetch(`${base}next`);
    const id = next.headers.get('Lambda-Runtime-Aws-Request-Id') ?? '';
    const event = (await next.json()) as InvokeEventJson;
    const result = await handle(event);
    await fetch(`${base}${id}/response`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(result),
    });
  }
}
