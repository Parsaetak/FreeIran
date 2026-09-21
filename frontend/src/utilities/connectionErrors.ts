/**
 * v0.9.10 humanized connection failures (§5 Errors): raw technical
 * failures become a three-part explanation —
 *
 *     What happened → What FreeIran is doing → What you can do
 *
 * with the full technical detail available through an expandable
 * section (never hidden, never silently dropped). Security and trust
 * failures (untrusted-route policy, verification) keep their full
 * meaning: they are explained, never weakened.
 */

export interface HumanizedConnectionError {
  /** Plain-language description of what happened. */
  whatHappened: string;
  /** What the application is doing about it right now (honest). */
  doingNow: string;
  /** Concrete next steps the user can take. */
  canDo: string[];
  /** The raw technical error (expandable details section). */
  technical: string;
  /** Whether the built-in recovery is already handling it. */
  recovering: boolean;
}

/** Never-guess fallback when classification fails. */
function fallback(raw: string, technical: string): HumanizedConnectionError {
  return {
    whatHappened:
      "The connection could not be established. " +
      "The technical explanation below has the exact reason.",
    doingNow:
      "FreeIran recorded this failure and put the failed route on cooldown, " +
      "so it will not be picked again immediately.",
    canDo: [
      "Try Connect again — routes change constantly",
      "Open Configurations and test a specific configuration",
      "If it keeps failing, check Sources and refresh, then retry",
    ],
    technical: raw || technical || "no error detail available",
    recovering: false,
  };
}

/**
 * Classify a connection error into the human model. The raw text is
 * matched against the engine's classified failure surface (engine/errors
 * + connection manager verdicts) — the mapping stays on the frontend,
 * the engine's own humanize layer is respected first.
 */
export function humanizeConnectionError(message: string, technical = ""): HumanizedConnectionError {
  const raw = message || technical;
  const text = raw.toLowerCase();

  // Trust boundary — keep the full security meaning.
  if (text.includes("untrusted")) {
    return {
      whatHappened:
        "Every available route comes from public, untrusted sources, and " +
        "automatic connection only uses trusted routes.",
      doingNow:
        "Nothing was connected — this is a deliberate safety decision, not a glitch.",
      canDo: [
        "Open Configurations and connect a public route explicitly (your choice)",
        "Or open Settings → Connection and allow public untrusted routes for automatic use",
      ],
      technical: raw,
      recovering: false,
    };
  }

  if (text.includes("verification failed") || text.includes("verification (" ) || text.includes("not usable")) {
    return {
      whatHappened:
        "A route was started, but real Internet traffic could not pass through it. " +
        "FreeIran only reports a connection as successful after verifying actual connectivity.",
      doingNow:
        "The unusable route was closed and its configuration moved to cooldown; " +
        "the next candidate in line is being tried.",
      canDo: [
        "Try Connect again to move to the next candidates",
        "Open Configurations → Test to refresh measurements first",
      ],
      technical: raw,
      recovering: false,
    };
  }

  if (text.includes("no backend attempt succeeded") || text.includes("no verified connection") || text.includes("no candidate")) {
    return {
      whatHappened:
        "None of the currently known configurations could establish a verified connection.",
      doingNow:
        "All failed candidates are on cooldown so they will not be retried immediately.",
      canDo: [
        "Open Sources and press Refresh now, then connect again",
        "Try the Tor or Psiphon route on the Connect screen",
        "Open Configurations and test connections to refresh the rankings",
      ],
      technical: raw,
      recovering: false,
    };
  }

  if (text.includes("not installed") || text.includes("no verified binary") || text.includes("core")) {
    return {
      whatHappened:
        "A connection engine (a protocol core) is required to run configurations, " +
        "and it is not installed or not usable yet.",
      doingNow: "The connection attempt stopped before starting any route.",
      canDo: [
        "Open More → Cores and install the suggested core (checksum-verified download)",
        "Then come back and press Connect",
      ],
      technical: raw,
      recovering: false,
    };
  }

  if (text.includes("port") && (text.includes("in use") || text.includes("range"))) {
    return {
      whatHappened:
        "The local network port the connection needs is occupied by another program " +
        "or outside the allowed range.",
      doingNow: "The attempt was stopped before it could conflict with other software.",
      canDo: [
        "Close other proxy/VPN applications and try again",
        "Or open Settings and pick a different local port",
      ],
      technical: raw,
      recovering: false,
    };
  }

  if (text.includes("timeout") || text.includes("timed out") || text.includes("did not become ready")) {
    return {
      whatHappened:
        "A route took too long to answer, so it was given up on. Slow or dead routes " +
        "are treated as failures.",
      doingNow:
        "The slow route was closed and recorded; automatic recovery can pick a faster one.",
      canDo: [
        "Try Connect again",
        "If this repeats, refresh sources and test configurations to renew the rankings",
      ],
      technical: raw,
      recovering: false,
    };
  }

  if (text.includes("already active")) {
    return {
      whatHappened: "A connection is already running — only one connection can be active at a time.",
      doingNow: "The existing connection was left untouched.",
      canDo: ["Disconnect first, then connect again"],
      technical: raw,
      recovering: false,
    };
  }

  return fallback(raw, technical);
}

/**
 * Lightweight contextual education hints (§5 Education): short,
 * one-line explanations shown where confusion is likely. Kept small —
 * the UI is not documentation.
 */
export const EDUCATION_HINTS = {
  configuration:
    "A configuration is one set of connection details for a single server. " +
    "FreeIran collects them from sources and tests which ones actually work.",
  source:
    "A source is a public list that configurations are collected from. " +
    "Refreshing a source looks for new configurations.",
  verification:
    "Verified means real Internet traffic passed through the route — " +
    "not just that the connection started.",
  core:
    "A core is the engine that runs a configuration. Different protocols " +
    "need different cores (Xray, V2Ray, sing-box).",
  favorite:
    "Favorites are configurations you saved for quick access. " +
    "They still go through the same testing and verification as everything else.",
  routeRejected:
    "Routes can be rejected when they fail testing, come from untrusted " +
    "sources under automatic selection, or are on cooldown after a failure.",
} as const;
