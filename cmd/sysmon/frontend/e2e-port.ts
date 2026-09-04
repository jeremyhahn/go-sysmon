// Port selection for the end-to-end suite.
//
// The suite used to serve on a constant :18080, which made two Browser Tests
// jobs on one runner collide by construction: whichever bound second failed
// with "18080 is already used" on a machine where nothing was wrong. Measured
// on 2026-09-03, every failed Browser Tests job in the last hundred CI runs was
// this and nothing else -- four of four, on four different runners, three of
// them on `main`. One of those reds was a commit that changed 28 lines of
// markdown (issue #62).
//
// Two properties are wanted, and only the second is obvious:
//
//   1. Two jobs running at once must not choose the same port.
//   2. Nothing else on the host may be holding the port when the server binds.
//
// (2) is why this does not probe for a free port. Playwright evaluates this
// config, then runs `npm run build` and `go build` -- a minute or more --
// before the server binds. A port that was free at the probe is not a port
// that is free at the bind, and while it sits unclaimed the kernel is free to
// hand it to any outbound connection on the box as a source port. The whole
// band below is therefore chosen to sit UNDER the ephemeral range
// (/proc/sys/net/ipv4/ip_local_port_range, 32768-60999 by default), where the
// kernel never allocates on its own.
//
// (1) is met by deriving the port from the identity of the CI job rather than
// from chance: two jobs on one runner always differ in at least one of run id,
// attempt, job name or runner name, so they land on different ports without
// coordinating. Deterministic also means a re-run of a job reproduces its port,
// which is what makes a failure reproducible.

/** The historical fixed port, kept for local runs where no CI identity exists. */
export const localPort = 18080;

/** First port of the band, chosen to sit below the kernel's ephemeral range. */
export const bandStart = 20000;

/** Size of the band; 20000-29999 stays clear of 32768 on every default Linux. */
export const bandSize = 10000;

/** The environment variables that identify one CI job among concurrent ones. */
const identityVars = ['GITHUB_RUN_ID', 'GITHUB_RUN_ATTEMPT', 'GITHUB_JOB', 'RUNNER_NAME'];

type Env = Record<string, string | undefined>;

/**
 * e2ePort returns the port the end-to-end server should bind.
 *
 * An explicit SYSMON_E2E_PORT wins, so a developer can pin one. With no CI
 * identity in the environment the historical port is returned unchanged --
 * local behaviour does not move, and `reuseExistingServer` still finds a
 * hand-started server where it always did.
 */
export function e2ePort(env: Env = process.env): number {
	const override = Number(env.SYSMON_E2E_PORT);
	if (Number.isInteger(override) && override > 0 && override < 65536) {
		return override;
	}

	const identity = identityVars
		.map((name) => env[name])
		.filter((value): value is string => Boolean(value))
		.join('|');

	if (identity === '') {
		return localPort;
	}

	return bandStart + (fnv1a(identity) % bandSize);
}

/**
 * fnv1a is the 32-bit FNV-1a hash. It is used here for spread, not for
 * secrecy: what matters is that two identities differing in one character land
 * far apart, which a sum of char codes would not.
 */
function fnv1a(input: string): number {
	let hash = 0x811c9dc5;
	for (let i = 0; i < input.length; i++) {
		hash ^= input.charCodeAt(i);
		// The shifts are the FNV prime, 16777619, as additions -- Math.imul
		// keeps the product in 32 bits.
		hash = Math.imul(hash, 0x01000193);
	}
	return hash >>> 0;
}
