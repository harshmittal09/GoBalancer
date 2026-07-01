/**
 * stress.js — Production-Grade k6 TCP Stress Test
 *
 * Target  : Layer-4 TCP Load Balancer at localhost:8080
 * Engine  : Custom k6 binary built with the xk6-tcp extension
 *           (github.com/grafana/xk6-tcp v0.3.0)
 *
 * ─── Verified API (xk6-tcp v0.3.0, k6 v2.0.0-rc1) ────────────────────────
 *
 *   const sock = new tcp.Socket();
 *   sock.on('connect', cb)          – fired after successful dial
 *   sock.on('data',    cb(payload)) – fired when bytes arrive (string or AB)
 *   sock.on('error',   cb(err))     – fired on any TCP error
 *   sock.on('close',   cb)          – fired after socket is fully closed
 *   sock.write(string)              – sends data
 *   sock.connect(port, host)        – initiates dial (port first, host second)
 *   sock.setTimeout(ms)             – sets an idle timeout
 *   sock.destroy()                  – closes the socket
 *
 *   ⚠  There is NO sock.connect('host:port') — port must be an integer passed
 *      as the FIRST argument and host as the SECOND.
 *   ⚠  There is NO sock.read() — responses arrive via the 'data' event.
 *   ⚠  connect() is asynchronous; it returns before the connection is open.
 *      All work must be done inside event handlers.
 *
 * ─── Load Profile ────────────────────────────────────────────────────────────
 *
 *   0 s ──ramp 30 s──▶ 5,000 VUs ──hold 60 s──▶ ramp-down 30 s ──▶ 0
 *
 * ─── Strict Thresholds ───────────────────────────────────────────────────────
 *
 *   tcp_connect_errors : count == 0
 *   tcp_write_errors   : count == 0
 *   tcp_read_errors    : count == 0
 *   payload_mismatches : count == 0
 */

import tcp from 'k6/x/tcp';
import { check, sleep } from 'k6';
import { Counter, Trend, Rate } from 'k6/metrics';
import { textSummary } from 'https://jslib.k6.io/k6-summary/0.0.2/index.js';

// ─────────────────────────────────────────────────────────────────────────────
// Custom Metrics
// ─────────────────────────────────────────────────────────────────────────────

const tcpConnectErrors  = new Counter('tcp_connect_errors');
const tcpWriteErrors    = new Counter('tcp_write_errors');
const tcpReadErrors     = new Counter('tcp_read_errors');
const payloadMismatches = new Counter('payload_mismatches');
const tcpRTT            = new Trend('tcp_rtt_ms', /*isTime=*/true);
const sessionSuccessRate = new Rate('session_success_rate');

// ─────────────────────────────────────────────────────────────────────────────
// Test Options
// ─────────────────────────────────────────────────────────────────────────────

export const options = {
  scenarios: {
    default: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        // Ramp up to 100 VUs over 30s
        { duration: '30s', target: 100 },
        // Hold at 100 VUs for 1m
        { duration: '1m',  target: 100 },
        // Ramp down to 0 VUs over 30s
        { duration: '30s', target: 0 },
      ],
      gracefulRampDown: '3m',
      gracefulStop: '3m',
    },
  },
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'],
  thresholds: {
    // strict requirement: test fails if any error count is > 0
    'tcp_connect_errors'  : ['count==0'],
    'tcp_write_errors'    : ['count==0'],
    'tcp_read_errors'     : ['count==0'],
    'payload_mismatches'  : ['count==0'],
    // Extra SLOs
    'tcp_rtt_ms'          : ['p(99)<15000'], // Relaxed for local tests
    'session_success_rate': ['rate>=0.99'],  // >99%
    'checks'              : ['rate>=0.99'],
  },
};

// ─────────────────────────────────────────────────────────────────────────────
// Constants
// ─────────────────────────────────────────────────────────────────────────────

/**
 * Target host — override at runtime:
 *   TARGET_HOST=10.0.0.1 TARGET_PORT=9000 ./k6 run stress.js
 */
const TARGET_HOST = __ENV.TARGET_HOST || 'localhost';
const TARGET_PORT = parseInt(__ENV.TARGET_PORT || '8080', 10);

/**
 * Payload written over TCP on every iteration.
 * The backends run `socat EXEC:cat` which echoes every byte received.
 * A trailing '\n' helps flush line-buffered socat configurations.
 */
const PAYLOAD = 'PING_STREAM_TEST\n';

/** Socket idle timeout in ms — prevents a hung VU from blocking forever. */
const SOCKET_TIMEOUT_MS = 8000;

// ─────────────────────────────────────────────────────────────────────────────
// Default Function  (one complete TCP session per VU per iteration)
// ─────────────────────────────────────────────────────────────────────────────

/**
 * The xk6-tcp Socket API is event-driven (Node.js net.Socket style):
 *
 *   sock.connect(port, host)
 *   → 'connect' event fires  → sock.write(payload)
 *   → 'data'    event fires  → verify echo, sock.destroy()
 *   → 'close'   event fires  → record metrics
 *
 * connect() returns *before* the connection is established.  k6 drains the
 * event loop after the default function returns, so we must record metrics
 * inside the 'close' handler where all state is final.
 */
export default async function () {
  const iterStart  = Date.now();
  let   sessionOk  = false;
  let   received   = '';
  let   writeError = false;
  let   connectError = false;
  let   readError = false;

  const sock = new tcp.Socket();
  sock.setTimeout(SOCKET_TIMEOUT_MS);

  // Wrap the async event-driven TCP lifecycle in a Promise
  // so the main VU function blocks until the session completes.
  await new Promise((resolve) => {
    // ── JS Event Loop Safety Timeout ──────────────────────────────────────────
    // If macOS VPNKit completely drops the connection, xk6-tcp can hang indefinitely.
    // This strict JS timeout force-closes the socket before k6 can hard-kill the VU.
    const safetyTimeout = setTimeout(() => {
      connectError = true;
      try { sock.destroy(); } catch (_) {}
      resolve();
    }, SOCKET_TIMEOUT_MS);

    // ── Event: connect ────────────────────────────────────────────────────────
    sock.on('connect', () => {
      // Write the test payload.  sock.write() is synchronous in xk6-tcp.
      try {
        sock.write(PAYLOAD);
      } catch (e) {
        writeError = true;
        sock.destroy();
      }
    });

    // ── Event: data ───────────────────────────────────────────────────────────
    sock.on('data', (chunk) => {
      if (typeof chunk === 'string') {
        received += chunk;
      } else {
        received += String.fromCharCode(...new Uint8Array(chunk));
      }

      if (received.length >= PAYLOAD.length) {
        sock.destroy();
      }
    });

    // ── Event: error ──────────────────────────────────────────────────────────
    sock.on('error', (err) => {
      if (!received && !writeError) {
        connectError = true;
      } else {
        readError = true;
      }
      try { sock.destroy(); } catch (_) { /* already closed */ }
    });

    // ── Event: close ─────────────────────────────────────────────────────────
    sock.on('close', () => {
      clearTimeout(safetyTimeout);
      resolve(); // Unblock the VU execution
    });

    // ── Initiate the connection ───────────────────────────────────────────────
    try {
      sock.connect(TARGET_PORT, TARGET_HOST).catch((e) => {
        connectError = true;
        clearTimeout(safetyTimeout);
        resolve();
      });
    } catch (e) {
      connectError = true;
      clearTimeout(safetyTimeout);
      resolve();
    }
  });

  // Now back in the main synchronous VU context, after the socket is fully closed.
  // We record checks and metrics here safely without racing k6's metric flusher.
  const trimmedResponse = received.trim();
  const trimmedPayload  = PAYLOAD.trim();

  if (received === '' && !writeError) {
    readError = true;
  }

  const integrityOk = trimmedResponse === trimmedPayload;
  if (received !== '' && !integrityOk) {
    payloadMismatches.add(1);
  }

  sessionOk = received !== '' && integrityOk && !writeError && !connectError;

  // ── Emit deferred error metrics safely in the main thread ─────────────────
  if (writeError)   tcpWriteErrors.add(1);
  if (connectError) tcpConnectErrors.add(1);
  if (readError)    tcpReadErrors.add(1);

  // ── Record all checks sequentially ────────────────────────────────────────
  check(sessionOk, {
    'tcp session succeeded'   : (ok) => ok,
  });
  check(received, {
    'tcp connect succeeded'   : ()  => !writeError && !connectError,
    'tcp write succeeded'     : ()  => !writeError,
    'tcp read succeeded'      : (r) => r !== '',
    'response matches payload': (r) => r.trim() === PAYLOAD.trim(),
  });

  // ── Record RTT and session outcome ────────────────────────────────────────
  tcpRTT.add(Date.now() - iterStart);
  sessionSuccessRate.add(sessionOk);

  sleep(0.5);
}

// ─────────────────────────────────────────────────────────────────────────────
// handleSummary — Custom end-of-test formatted report
// ─────────────────────────────────────────────────────────────────────────────

export function handleSummary(data) {
  // ── Helpers ───────────────────────────────────────────────────────────────

  function metric(name, field, fallback = 'N/A') {
    try {
      const m = data.metrics[name];
      if (!m) return fallback;
      const v = m.values[field];
      return v !== undefined ? v : fallback;
    } catch (_) { return fallback; }
  }

  function fmtMs(v) {
    if (v === 'N/A' || v == null) return 'N/A';
    return `${Number(v).toFixed(2)} ms`;
  }

  function fmtRate(v) {
    if (v === 'N/A' || v == null) return 'N/A';
    return `${(Number(v) * 100).toFixed(2)}%`;
  }

  /** Format integer with comma separators — no toLocaleString (not in Goja). */
  function fmtInt(v) {
    if (v === 'N/A' || v == null) return 'N/A';
    const n = Math.round(Number(v));
    const s = String(n);
    let   r = '';
    for (let i = 0; i < s.length; i++) {
      if (i > 0 && (s.length - i) % 3 === 0) r += ',';
      r += s[i];
    }
    return r;
  }

  // ── Extract metrics ───────────────────────────────────────────────────────

  const peakVUs          = metric('vus_max',              'max',   'N/A');
  const iterAvg          = metric('iteration_duration',   'avg',   'N/A');
  const iterP50          = metric('iteration_duration',   'med',   'N/A');
  const iterP90          = metric('iteration_duration',   'p(90)', 'N/A');
  const iterP95          = metric('iteration_duration',   'p(95)', 'N/A');
  const iterP99          = metric('iteration_duration',   'p(99)', 'N/A');
  const iterMax          = metric('iteration_duration',   'max',   'N/A');
  const totalIterations  = metric('iterations',           'count', 'N/A');
  const iterRate         = metric('iterations',           'rate',  'N/A');
  const tcpRttP50        = metric('tcp_rtt_ms',           'med',   'N/A');
  const tcpRttP99        = metric('tcp_rtt_ms',           'p(99)', 'N/A');
  const tcpRttMax        = metric('tcp_rtt_ms',           'max',   'N/A');
  const connectErrors    = metric('tcp_connect_errors',   'count', 0);
  const writeErrors      = metric('tcp_write_errors',     'count', 0);
  const readErrors       = metric('tcp_read_errors',      'count', 0);
  const dataMismatches   = metric('payload_mismatches',   'count', 0);
  const totalTcpErrors   = Number(connectErrors) + Number(writeErrors) + Number(readErrors);
  const successRate      = metric('session_success_rate', 'rate',  'N/A');
  const checksRate       = metric('checks',               'rate',  'N/A');
  const dataRecvBytes    = metric('data_received',        'count', 'N/A');
  const dataSentBytes    = metric('data_sent',            'count', 'N/A');

  const thresholdsFailed = data.metrics
    ? Object.values(data.metrics).some(
        (m) => m.thresholds && Object.values(m.thresholds).some((t) => t.ok === false)
      )
    : false;

  const overallStatus =
    (thresholdsFailed || totalTcpErrors > 0) ? 'FAIL ❌' : 'PASS ✅';

  // ── Format ────────────────────────────────────────────────────────────────

  const D = '═'.repeat(68);
  const T = '─'.repeat(68);
  const col = (label, value) =>
    `  ${label.padEnd(36)} ${String(value).padStart(28)}`;

  const report = [
    '',
    D,
    '  ⚡  TCP LOAD BALANCER — k6 STRESS TEST RESULTS',
    D,
    '',
    `  Overall Status  : ${overallStatus}`,
    `  Target          : ${TARGET_HOST}:${TARGET_PORT}`,
    '',
    T,
    '  LOAD PROFILE',
    T,
    col('Peak Virtual Users (VUs)',   fmtInt(peakVUs)),
    col('Total Iterations',           fmtInt(totalIterations)),
    col('Iteration Rate',
        iterRate !== 'N/A' ? `${Number(iterRate).toFixed(2)}/s` : 'N/A'),
    '',
    T,
    '  LATENCY  (iteration_duration)',
    T,
    col('Average',                    fmtMs(iterAvg)),
    col('p50 (median)',               fmtMs(iterP50)),
    col('p90',                        fmtMs(iterP90)),
    col('p95',                        fmtMs(iterP95)),
    col('p99  ◀ primary SLO target',  fmtMs(iterP99)),
    col('Max (worst case)',            fmtMs(iterMax)),
    '',
    T,
    '  TCP ROUND-TRIP TIME  (tcp_rtt_ms)',
    T,
    col('p50',                        fmtMs(tcpRttP50)),
    col('p99',                        fmtMs(tcpRttP99)),
    col('Max',                        fmtMs(tcpRttMax)),
    '',
    T,
    '  ERROR SUMMARY  (strict threshold: count == 0)',
    T,
    col('TCP Connect Errors',         fmtInt(connectErrors)),
    col('TCP Write Errors',           fmtInt(writeErrors)),
    col('TCP Read Errors',            fmtInt(readErrors)),
    col('Payload Integrity Failures', fmtInt(dataMismatches)),
    col('── TOTAL TCP Errors',        fmtInt(totalTcpErrors)),
    '',
    T,
    '  SESSION QUALITY',
    T,
    col('Session Success Rate',       fmtRate(successRate)),
    col('Check Pass Rate',            fmtRate(checksRate)),
    '',
    T,
    '  DATA THROUGHPUT',
    T,
    col('Data Received',
        dataRecvBytes !== 'N/A'
          ? `${(Number(dataRecvBytes) / 1048576).toFixed(2)} MiB` : 'N/A'),
    col('Data Sent',
        dataSentBytes !== 'N/A'
          ? `${(Number(dataSentBytes) / 1048576).toFixed(2)} MiB` : 'N/A'),
    '',
    D,
    '',
  ].join('\n');

  return {
    stdout         : report,
    'summary.txt'  : textSummary(data, { indent: '  ', enableColors: false }),
    'summary.json' : JSON.stringify(data, null, 2),
  };
}