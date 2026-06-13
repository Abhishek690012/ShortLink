import http from "k6/http";
import { check, sleep } from "k6";
import { Counter, Rate, Trend } from "k6/metrics";
import { SharedArray } from "k6/data";

const BASE_URL = "http://localhost:8080";
const SEED_COUNT = 50;

const writeDuration = new Trend("shortlink_write_duration", true);
const readDuration = new Trend("shortlink_read_duration", true);
const serverErrors = new Counter("shortlink_server_errors");
const errorRate = new Rate("shortlink_error_rate");
const writeSuccess = new Counter("shortlink_write_success");
const readSuccess = new Counter("shortlink_read_success");

export const options = {
  scenarios: {
    write_stress: {
      executor: "ramping-vus",
      startVUs: 1,
      stages: [
        { duration: "15s", target: 10 },
        { duration: "30s", target: 50 },
        { duration: "30s", target: 50 },
        { duration: "15s", target: 0 },
      ],
      gracefulRampDown: "10s",
      exec: "writeStress",
      tags: { scenario: "write_stress" },
    },
    read_stress: {
      executor: "ramping-arrival-rate",
      startRate: 50,
      timeUnit: "1s",
      preAllocatedVUs: 20,
      maxVUs: 200,
      stages: [
        { duration: "15s", target: 50 },
        { duration: "30s", target: 300 },
        { duration: "30s", target: 300 },
        { duration: "15s", target: 0 },
      ],
      startTime: "95s",
      exec: "readStress",
      tags: { scenario: "read_stress" },
    },
    mixed_workload: {
      executor: "ramping-vus",
      startVUs: 1,
      stages: [
        { duration: "20s", target: 30 },
        { duration: "60s", target: 30 },
        { duration: "10s", target: 0 },
      ],
      startTime: "205s",
      exec: "mixedWorkload",
      tags: { scenario: "mixed_workload" },
    },
  },
  thresholds: {
    http_req_duration: ["p(99)<2000"],
    "http_req_duration{expected_response:true}": ["p(95)<500"],
    http_req_failed: ["rate<0.01"],
    shortlink_write_duration: ["p(95)<800", "p(99)<1500"],
    shortlink_read_duration: ["p(95)<200", "p(99)<500"],
    shortlink_server_errors: ["count<10"],
    shortlink_error_rate: ["rate<0.01"],
  },
};

const seededIDs = new SharedArray("seededIDs", function () {
  return [];
});

export function setup() {
  console.log(`[setup] Seeding ${SEED_COUNT} short URLs...`);
  const ids = [];
  const headers = { "Content-Type": "application/json" };

  for (let i = 0; i < SEED_COUNT; i++) {
    const body = JSON.stringify({
      url: `https://setup.example.com/seed-path/${i}?ts=${Date.now()}&r=${Math.random()}`,
    });
    const res = http.post(`${BASE_URL}/shorten`, body, { headers });

    if (res.status === 201) {
      try {
        const payload = JSON.parse(res.body);
        if (payload.id) ids.push(payload.id);
      } catch (e) {
        console.warn(`[setup] Failed to parse body: ${e}`);
      }
    } else {
      console.warn(`[setup] Seed ${i} failed status ${res.status}`);
    }
    sleep(0.05);
  }

  if (ids.length === 0) {
    throw new Error("[setup] No IDs seeded. Check server/DB health.");
  }
  return { ids };
}

export function writeStress(data) {
  const headers = { "Content-Type": "application/json" };
  const uniqueURL = `https://write-stress.example.com/path/${__VU}-${__ITER}?t=${Date.now()}`;
  const body = JSON.stringify({ url: uniqueURL });

  const res = http.post(`${BASE_URL}/shorten`, body, {
    headers,
    tags: { endpoint: "shorten" },
  });

  writeDuration.add(res.timings.duration);

  const ok = check(res, {
    "write: status is 201": (r) => r.status === 201,
    "write: has id": (r) => {
      try {
        return JSON.parse(r.body).id !== undefined;
      } catch (_) {
        return false;
      }
    },
  });

  if (ok) {
    writeSuccess.add(1);
    errorRate.add(0);
  } else {
    errorRate.add(1);
    if (res.status >= 500) serverErrors.add(1);
  }
  sleep(Math.random() * 0.1);
}

export function readStress(data) {
  const ids = data && data.ids && data.ids.length > 0 ? data.ids : null;
  if (!ids) return;

  const id = ids[Math.floor(Math.random() * ids.length)];
  const res = http.get(`${BASE_URL}/r/${id}`, {
    redirects: 0, // Measure redirect latency only, do not follow
    tags: { endpoint: "redirect" },
  });

  readDuration.add(res.timings.duration);

  const ok = check(res, {
    "read: status is 302": (r) => r.status === 302,
    "read: location header present": (r) =>
      r.headers["Location"] !== undefined || r.headers["location"] !== undefined,
  });

  if (ok) {
    readSuccess.add(1);
    errorRate.add(0);
  } else {
    errorRate.add(1);
    if (res.status >= 500) serverErrors.add(1);
  }
  sleep(Math.random() * 0.05);
}

export function mixedWorkload(data) {
  if (Math.random() < 0.8) {
    readStress(data);
  } else {
    writeStress(data);
  }
}

export function teardown(data) {
  const seedCount = data && data.ids ? data.ids.length : 0;
  console.log(`[teardown] Seeded IDs used for read tests: ${seedCount}`);
}

