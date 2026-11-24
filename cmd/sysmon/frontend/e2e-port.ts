// Port selection for the end-to-end suite.
//
// The port is derived from the CI job's identity (run id, attempt, job name,
// runner name), so concurrent jobs on one runner differ and a re-run
// reproduces its port. It is not probed: the server binds a minute or more
// after this config is evaluated. The band sits below the kernel's ephemeral
// range (32768-60999 by default), so no outbound connection takes it.

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
