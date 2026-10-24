const labels: Record<string, string> = {
  running: "Running",
  stopped: "Stopped",
  "no-container": "No container",
  "waiting-for-answers": "Awaiting answers",
  succeeded: "Succeeded",
  failed: "Failed",
  ended: "Ended",
};

/** A state shown as an inked stamp; unknown states are shown verbatim. */
export function StateStamp({ state, detail }: { state: string; detail?: string }) {
  return (
    <span className={`stamp stamp-${state.replace(/[^a-z-]/g, "")}`} data-state={state}>
      <span className="stamp-label">{labels[state] ?? state}</span>
      {detail ? <span className="stamp-detail">{detail}</span> : null}
    </span>
  );
}
