import { own } from "../own.ts";

const labels: Record<string, string> = {
  running: "Running",
  stopped: "Stopped",
  "no-container": "No container",
  "no-folder": "No folder",
  "waiting-for-answers": "Awaiting answers",
  succeeded: "Succeeded",
  failed: "Failed",
  ended: "Ended",
};

/**
 * A state shown as a status badge; unknown states are shown verbatim, or as
 * label when one is given.
 */
export function StatusBadge({
  state,
  detail,
  label,
}: {
  state: string;
  detail?: string;
  label?: string;
}) {
  return (
    <span className={`status status-${state.replace(/[^a-z-]/g, "")}`} data-state={state}>
      <span className="status-label">{label ?? own(labels, state) ?? state}</span>
      {detail ? <span className="status-detail">{detail}</span> : null}
    </span>
  );
}
