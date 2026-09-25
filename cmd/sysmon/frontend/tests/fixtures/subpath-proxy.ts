import http from 'node:http';
import type { AddressInfo } from 'node:net';

/** A reverse proxy serving the dashboard under a sub-path, and what it saw. */
export interface SubpathProxy {
  /** The dashboard's URL through the proxy, ending in the prefix and a slash. */
  url: string;
  /** Every request path that arrived outside the prefix, which it refused. */
  outside: string[];
  /** Every request path it forwarded, as the client asked for it. */
  forwarded: string[];
  close(): Promise<void>;
}

/**
 * Starts a reverse proxy that serves `upstream` under `prefix` the way an
 * operator's nginx or Traefik does: /sysmon/api/events is forwarded as
 * /api/events, and the response -- a stream included -- is passed back as it
 * arrives. A request outside the prefix is answered 404 and recorded, since in
 * a real deployment it reaches some other application or nothing.
 *
 * It listens on port 0, so no two runs can collide; the port is read back.
 */
export async function startSubpathProxy(upstream: string, prefix: string): Promise<SubpathProxy> {
  const target = new URL(upstream);
  const outside: string[] = [];
  const forwarded: string[] = [];

  const server = http.createServer((req, res) => {
    const path = req.url ?? '/';
    if (!path.startsWith(prefix + '/')) {
      outside.push(path);
      res.writeHead(404, { 'Content-Type': 'text/plain' });
      res.end('not under ' + prefix);
      return;
    }
    forwarded.push(path);
    const upstreamReq = http.request(
      {
        host: target.hostname,
        port: target.port,
        method: req.method,
        path: path.slice(prefix.length),
        headers: { ...req.headers, host: target.host },
      },
      (upstreamRes) => {
        res.writeHead(upstreamRes.statusCode ?? 502, upstreamRes.headers);
        upstreamRes.pipe(res);
      },
    );
    upstreamReq.on('error', () => {
      if (!res.headersSent) res.writeHead(502);
      res.end();
    });
    // A client that goes away -- the page navigating, or an EventSource being
    // closed -- ends the upstream request too, as a real proxy does.
    res.on('close', () => upstreamReq.destroy());
    req.pipe(upstreamReq);
  });

  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;

  return {
    url: `http://127.0.0.1:${port}${prefix}/`,
    outside,
    forwarded,
    close: () =>
      new Promise<void>((resolve, reject) => {
        server.closeAllConnections();
        server.close((err) => (err ? reject(err) : resolve()));
      }),
  };
}
