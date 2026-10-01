import { useCallback, useEffect, useMemo, useState } from "react";
import { IconCheck, IconChevronDown, IconRefresh, IconX, IconZap } from "./Icons";
import { call, connectionService, dataService, proxyChainService, testQueueService } from "../services";
import type { ProxyChainDetails } from "../services";
import { useConnectionStore } from "../state/connectionStore";
import { describeError, toast } from "../state/toastStore";
import { formatLatency } from "../utilities/format";

/**
 * ChainEditor (v0.12.2): the compact desktop editor for proxy chains —
 * an ordered hop list over EXISTING configurations, with the compiled
 * preview (A → B → C), validation before save, and the honest check
 * actions. Hops are referenced by ID; the underlying configurations
 * are never copied, edited or deleted by the chain.
 */

interface ChainEditorProps {
  /** Editor visibility. */
  open: boolean;
  /** The chain to edit, or null to create a new one. */
  chain: ProxyChainDetails | null;
  /** Configuration IDs pre-seeded into the draft ("build from selected"). */
  initialHops?: string[];
  onClose: () => void;
  onSaved: (chainID: string) => void;
}

/** One editor hop row: id + display facts. */
interface DraftHop {
  id: string;
  name: string;
  protocol: string;
  endpoint: string;
  latency: string;
}

interface CandidateConfig {
  id: string;
  name: string;
  type: string;
  address: string;
  port: number;
  latency_ms?: number;
  working?: boolean;
}

export function ChainEditor({ open, chain, initialHops, onClose, onSaved }: ChainEditorProps) {
  const [name, setName] = useState("");
  const [hops, setHops] = useState<DraftHop[]>([]);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [checking, setChecking] = useState(false);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [pickerQuery, setPickerQuery] = useState("");
  const [candidates, setCandidates] = useState<CandidateConfig[]>([]);

  // Seed the draft from the edited chain (or reset for create).
  useEffect(() => {
    if (!open) return;

    setError("");
    setSaving(false);
    setChecking(false);
    setPickerOpen(false);
    setPickerQuery("");

    if (chain) {
      setName(chain.name);
      setHops(
        chain.hops.map((hop) => ({
          id: hop.config_id,
          name: hop.name || hop.config_id,
          protocol: hop.protocol || "",
          endpoint: `${hop.address || "?"}:${hop.port ?? "?"}`,
          latency: hop.tested_at ? (hop.working ? formatLatency(hop.latency_ms ?? 0) : "failed") : "untested",
        })),
      );
    } else {
      setName("");
      setHops([]);
    }
  }, [open, chain]);

  // Candidate pool: the first page of the store (bounded, server-side).
  // When the editor opens in create mode with pre-seeded hop ids
  // ("build from selected"), resolve them into draft rows first.
  useEffect(() => {
    if (!open || !pickerOpen || candidates.length > 0) return;

    void (async () => {
      try {
        const page = await call(() =>
          dataService.ListConfigsFiltered(
            {
              protocol: undefined,
              status: undefined,
              query: pickerQuery || undefined,
              sort_by: "latency",
              sort_desc: false,
              source: undefined,
              backend: undefined,
              group: undefined,
              ids: initialHops && initialHops.length > 0 ? initialHops : undefined,
            },
            0,
            200,
          ),
        );

        const loaded = (page?.items ?? []) as CandidateConfig[];

        setCandidates(loaded);

        if (initialHops && initialHops.length > 0) {
          setHops((current) => {
            if (current.length > 0) return current;

            const seeded = initialHops
              .map((id) => loaded.find((candidate) => candidate.id === id))
              .filter((candidate): candidate is CandidateConfig => Boolean(candidate))
              .map((candidate) => ({
                id: candidate.id,
                name: candidate.name || candidate.id,
                protocol: candidate.type,
                endpoint: `${candidate.address}:${candidate.port}`,
                latency: candidate.working ? formatLatency(candidate.latency_ms ?? 0) : "untested",
              }));

            return seeded.length > 0 ? seeded : current;
          });
        }
      } catch (e) {
        toast("error", "Could not load configurations", describeError(e));
      }
    })();
  }, [open, pickerOpen, pickerQuery, candidates.length, initialHops]);

  const maxHops = 4;

  const validationError = useMemo(() => {
    if (hops.length < 2) return "A proxy chain needs at least two hops.";
    if (hops.length > maxHops) return `A proxy chain is limited to ${maxHops} hops.`;

    const ids = new Set<string>();

    for (const hop of hops) {
      if (ids.has(hop.id)) return "Duplicate hops are not allowed.";

      ids.add(hop.id);
    }

    return "";
  }, [hops]);

  const addHop = useCallback(
    (candidate: CandidateConfig) => {
      setHops((current) => {
        if (current.length >= maxHops) {
          return current;
        }

        if (current.some((hop) => hop.id === candidate.id)) {
          return current;
        }

        return [
          ...current,
          {
            id: candidate.id,
            name: candidate.name || candidate.id,
            protocol: candidate.type,
            endpoint: `${candidate.address}:${candidate.port}`,
            latency: candidate.working ? formatLatency(candidate.latency_ms ?? 0) : "untested",
          },
        ];
      });
    },
    [],
  );

  const move = useCallback((index: number, delta: number) => {
    setHops((current) => {
      const next = [...current];
      const target = index + delta;

      if (target < 0 || target >= next.length) return current;

      const [hop] = next.splice(index, 1);
      next.splice(target, 0, hop);

      return next;
    });
  }, []);

  const remove = useCallback((index: number) => {
    setHops((current) => current.filter((_, i) => i !== index));
  }, []);

  const save = useCallback(async () => {
    setError("");

    if (!name.trim()) {
      setError("A chain name is required.");

      return;
    }

    if (validationError) {
      setError(validationError);

      return;
    }

    setSaving(true);

    try {
      const ids = hops.map((hop) => hop.id);

      if (chain) {
        // Update: apply the MINIMAL hop deltas through the SAME API —
        // remove dropped hops, insert new ones at the right position,
        // reorder to the draft sequence.
        await call(() => proxyChainService.RenameProxyChain(chain.id, name.trim()));

        const current = chain.config_ids;
        const target = ids;

        for (const id of current) {
          if (!target.includes(id)) {
            await call(() => proxyChainService.RemoveHop(chain.id, id));
          }
        }

        const surviving = current.filter((id) => target.includes(id));

        for (let position = 0; position < target.length; position++) {
          const id = target[position];

          if (!surviving.includes(id)) {
            await call(() => proxyChainService.AddHop(chain.id, id, position));
          } else {
            const currentIndex = surviving.indexOf(id);

            if (currentIndex !== position) {
              await call(() => proxyChainService.ReorderHop(chain.id, id, position));
            }
          }
        }

        onSaved(chain.id);
      } else {
        const created = await call(() => proxyChainService.CreateProxyChain(name.trim(), ids));

        onSaved(created.id);
      }

      toast("success", chain ? "Chain updated" : "Chain created", name.trim());
    } catch (e) {
      setError(describeError(e, "the chain could not be saved"));
    } finally {
      setSaving(false);
    }
  }, [chain, hops, name, onSaved, validationError]);

  const checkHops = useCallback(async () => {
    if (hops.length === 0) return;

    setChecking(true);

    try {
      await call(() =>
        testQueueService.EnqueueByFilter({
          scope: "selected",
          fingerprints: hops.map((hop) => hop.id),
          protocol: "",
          source: "",
          limit: 0,
          priority: 0,
          origin: "user",
        }),
      );

      toast("success", "Hop checks queued", "Results appear as the queue measures each hop.");
    } catch (e) {
      toast("error", "Could not queue hop checks", describeError(e));
    } finally {
      setChecking(false);
    }
  }, [hops]);

  const checkChain = useCallback(async () => {
    if (!chain) return;

    setChecking(true);

    try {
      const result = await call(() => proxyChainService.CheckChain(chain.id));

      if (result.end_to_end.ok) {
        toast("success", "Chain verified", `End-to-end ${result.end_to_end.ping_ms ?? 0} ms through ${result.end_to_end.backend}.`);
      } else {
        toast("error", "Chain check failed", result.end_to_end.last_error || "No usable end-to-end connection was verified.");
      }
    } catch (e) {
      toast("error", "Chain check failed", describeError(e));
    } finally {
      setChecking(false);
    }
  }, [chain]);

  const connect = useCallback(async () => {
    if (!chain) return;

    try {
      await call(() => connectionService.ConnectChain(chain.id));

      void useConnectionStore.getState().refresh().catch(() => undefined);
    } catch (e) {
      toast("error", "Chain connection failed", describeError(e));
    }
  }, [chain]);

  if (!open) return null;

  const preview = hops.map((hop) => hop.name).join(" → ");

  const filteredCandidates = candidates.filter((candidate) => {
    const id = candidate.id;
    const query = pickerQuery.trim().toLowerCase();

    if (!query) return true;

    return (
      candidate.name.toLowerCase().includes(query) ||
      candidate.address.toLowerCase().includes(query) ||
      id.toLowerCase().includes(query)
    );
  });

  return (
    <div className="dialog-overlay" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <div className="dialog chain-editor" role="dialog" aria-modal="true" aria-label="Proxy chain editor">
        <div className="dialog-header">
          <h3 className="dialog-title">{chain ? "Edit proxy chain" : "New proxy chain"}</h3>
          <button type="button" className="btn ghost icon" aria-label="Close dialog" onClick={onClose}>
            <IconX size={14} />
          </button>
        </div>

        <div className="dialog-body chain-editor-body">
          <label className="chain-name-row">
            <span>Name</span>
            <input
              type="text"
              value={name}
              placeholder="e.g. Home → VPS → exit"
              onChange={(event) => setName(event.target.value)}
            />
          </label>

          <ol className="chain-hops" aria-label="Ordered hops">
            {hops.map((hop, index) => (
              <li key={hop.id} className="chain-hop">
                <span className="chain-hop-index" aria-hidden>{index + 1}</span>
                <span className="chain-hop-main">
                  <span className="chain-hop-name" title={hop.id}>{hop.name}</span>
                  <span className="chain-hop-meta mono">
                    {hop.protocol} · {hop.endpoint} · {hop.latency}
                  </span>
                </span>
                <span className="chain-hop-actions">
                  <button type="button" className="btn sm ghost" disabled={index === 0} onClick={() => move(index, -1)} aria-label={`Move ${hop.name} up`}>
                    ↑
                  </button>
                  <button type="button" className="btn sm ghost" disabled={index === hops.length - 1} onClick={() => move(index, 1)} aria-label={`Move ${hop.name} down`}>
                    ↓
                  </button>
                  <button type="button" className="btn sm ghost" onClick={() => remove(index)} aria-label={`Remove ${hop.name}`}>
                    <IconX size={12} />
                  </button>
                </span>
              </li>
            ))}
            {hops.length === 0 && (
              <li className="chain-hop-empty">No hops yet — add two or more configurations.</li>
            )}
          </ol>

          <div className="chain-add-row">
            <button type="button" className="btn sm" disabled={hops.length >= maxHops} onClick={() => setPickerOpen((value) => !value)}>
              <IconChevronDown size={12} /> Add hop
            </button>
            <span className="chain-limit">{hops.length}/{maxHops} hops</span>
          </div>

          {pickerOpen && (
            <div className="chain-picker">
              <input
                type="text"
                value={pickerQuery}
                placeholder="Search configurations by name or address"
                onChange={(event) => setPickerQuery(event.target.value)}
              />
              <ul className="chain-picker-list" role="listbox" aria-label="Available configurations">
                {filteredCandidates.map((candidate) => {
                  const disabled = hops.some((hop) => hop.id === candidate.id) || hops.length >= maxHops;

                  return (
                    <li key={candidate.id}>
                      <button
                        type="button"
                        role="option"
                        aria-selected={false}
                        disabled={disabled}
                        onClick={() => addHop(candidate)}
                      >
                        <span>{candidate.name || candidate.id}</span>
                        <span className="mono muted">
                          {candidate.type} · {candidate.address}:{candidate.port}
                        </span>
                      </button>
                    </li>
                  );
                })}
                {filteredCandidates.length === 0 && <li className="chain-hop-empty">No matching configurations.</li>}
              </ul>
            </div>
          )}

          <div className="chain-preview" aria-label="Chain preview">
            {preview || "—"}
          </div>

          {error && (
            <div className="chain-error" role="alert">
              {error}
            </div>
          )}
        </div>

        <div className="dialog-footer chain-editor-footer">
          {chain && (
            <>
              <button type="button" className="btn sm" disabled={checking} onClick={() => void checkHops()}>
                <IconRefresh size={12} /> Check hops
              </button>
              <button type="button" className="btn sm" disabled={checking} onClick={() => void checkChain()}>
                <IconCheck size={12} /> Check chain
              </button>
              <button type="button" className="btn sm primary" onClick={() => void connect()}>
                <IconZap size={12} /> Connect
              </button>
              <span className="toolbar-divider" aria-hidden />
            </>
          )}
          <button type="button" className="btn sm" disabled={saving || Boolean(validationError)} onClick={() => void save()}>
            {saving ? <span className="btn-spinner" aria-hidden /> : null}
            Save chain
          </button>
          <button type="button" className="btn sm ghost" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    </div>
  );
}
