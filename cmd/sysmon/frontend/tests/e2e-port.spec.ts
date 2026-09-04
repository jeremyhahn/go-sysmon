import { test, expect } from '@playwright/test';
import { e2ePort, localPort, bandStart, bandSize } from '../e2e-port';

// The identities below are the real ones from the collision that produced
// issue #62: two Browser Tests jobs on vm-host-2, four minutes apart, both
// binding 18080. What these tests assert is that they no longer can.

const browserJobOn90 = {
	GITHUB_RUN_ID: '10645',
	GITHUB_RUN_ATTEMPT: '1',
	GITHUB_JOB: 'e2e',
	RUNNER_NAME: 'vm-host-2',
};

const browserJobOn87 = {
	GITHUB_RUN_ID: '10644',
	GITHUB_RUN_ATTEMPT: '1',
	GITHUB_JOB: 'e2e',
	RUNNER_NAME: 'vm-host-2',
};

test.describe('e2ePort', () => {
	test('two runs sharing a runner do not share a port', () => {
		expect(e2ePort(browserJobOn90)).not.toBe(e2ePort(browserJobOn87));
	});

	test('two jobs of one run do not share a port', () => {
		// Integration Tests and Browser Tests of the same commit are dispatched
		// to the same runner in the same second, routinely -- measured across a
		// hundred runs. They differ only in the job name.
		const integration = { ...browserJobOn90, GITHUB_JOB: 'integration' };
		expect(e2ePort(browserJobOn90)).not.toBe(e2ePort(integration));
	});

	test('a re-run of a job differs from the attempt it replaces', () => {
		// A re-run lands on the same runner while the previous attempt may still
		// be shutting down, which is the one case where a stale process really
		// can still hold the port.
		const secondAttempt = { ...browserJobOn90, GITHUB_RUN_ATTEMPT: '2' };
		expect(e2ePort(browserJobOn90)).not.toBe(e2ePort(secondAttempt));
	});

	test('the same job resolves to the same port every time', () => {
		// Determinism is what makes a failed run reproducible by hand.
		expect(e2ePort(browserJobOn90)).toBe(e2ePort({ ...browserJobOn90 }));
	});

	test('a local run keeps the historical port', () => {
		// No CI identity: behaviour must not move for a developer, whose
		// hand-started server is found by reuseExistingServer on this port.
		expect(e2ePort({})).toBe(localPort);
	});

	test('an explicit port wins over the derived one', () => {
		expect(e2ePort({ ...browserJobOn90, SYSMON_E2E_PORT: '31337' })).toBe(31337);
	});

	test('an unusable explicit port is ignored rather than obeyed', () => {
		// Every one of these would be a bind failure or a silent fallback to a
		// random port if it were passed through.
		for (const bad of ['', 'eight thousand', '0', '-1', '70000', '18080.5']) {
			const port = e2ePort({ ...browserJobOn90, SYSMON_E2E_PORT: bad });
			expect(port, `SYSMON_E2E_PORT=${JSON.stringify(bad)}`).toBe(e2ePort(browserJobOn90));
		}
	});

	test('every derived port stays below the kernel ephemeral range', () => {
		// 32768 is the low end of ip_local_port_range on a default Linux. Above
		// it the kernel may hand our port to an outbound connection during the
		// minute the suite spends building, which is precisely the failure a
		// probe-for-a-free-port strategy would still have.
		for (let run = 0; run < 2000; run++) {
			const port = e2ePort({ ...browserJobOn90, GITHUB_RUN_ID: String(run) });
			expect(port).toBeGreaterThanOrEqual(bandStart);
			expect(port).toBeLessThan(bandStart + bandSize);
			expect(port).toBeLessThan(32768);
		}
	});

	test('ports spread across the band rather than clustering', () => {
		// A hash that ignored most of its input would pass every test above and
		// still collide constantly in production. 1000 consecutive run ids
		// through a 10000-port band should collide a handful of times at most.
		const ports = new Set<number>();
		for (let run = 100000; run < 101000; run++) {
			ports.add(e2ePort({ ...browserJobOn90, GITHUB_RUN_ID: String(run) }));
		}
		expect(ports.size).toBeGreaterThanOrEqual(940);
	});
});
