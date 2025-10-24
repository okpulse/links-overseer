(function(){
  const $ = s => document.querySelector(s);
  const $$ = s => Array.from(document.querySelectorAll(s));

  let JOB_ID = null;
  let ALL_RESULTS = [];
  let STOPPED = false;
  let MODE = 'all';

  $("#startBtn").onclick = async function () {
    STOPPED = false;
    const start_url = $("#startUrl").value.trim();
    var depth = parseInt($("#depth").value || "2", 10);
    if (isNaN(depth)) depth = 2;
    depth = Math.max(0, Math.min(5, depth));
    const respect_robots = $("#respectRobots").checked;
    if (!/^https?:\/\/?/i.test(start_url)) { alert("Введите корректный URL, начиная с http(s)://"); return; }

    $("#progress").classList.remove("hidden");
    updateProgress({});

    try {
      const res = await fetch("/api/start", {
        method: "POST",
        headers: {"Content-Type":"application/json"},
        body: JSON.stringify({ start_url: start_url, depth: depth, respect_robots: respect_robots })
      });
      if (!res.ok) { alert("Ошибка запуска"); return; }
      const payload = await res.json();
      JOB_ID = payload.job_id;
      ALL_RESULTS = [];
      pollStatus();
    } catch(e) {
      console.warn("start error:", e);
    }
  };

  $("#stopBtn").onclick = async function(){
    if (!JOB_ID) return;
    STOPPED = true;
    try{ await fetch("/api/stop?job=" + encodeURIComponent(JOB_ID), {method:"POST"}); }catch(e){}
  };

  $("#filterChips").addEventListener("click", function(ev){
    const btn = ev.target.closest(".chip");
    if (!btn) return;
    $$(".chip").forEach(b => b.classList.remove("active"));
    btn.classList.add("active");
    MODE = btn.getAttribute("data-mode");
    renderTable();
  });

  async function pollStatus() {
    if (!JOB_ID || STOPPED) return;
    try {
      const res = await fetch("/api/status?job=" + encodeURIComponent(JOB_ID));
      if (!res.ok) return;
      const st = await res.json();
      updateProgress(st);

      try {
        const res2 = await fetch("/api/results?job=" + encodeURIComponent(JOB_ID));
        if (res2.ok) {
          ALL_RESULTS = await res2.json();
          renderTable();
        }
      } catch(e) {}

      if (st.state === "done" || st.state === "failed" || st.state === "canceled") { updateProgress(st); return; }
      setTimeout(pollStatus, 800);
    } catch(e) {
      console.warn("pollStatus error:", e);
    }
  }

  function updateProgress(st){
    var pagesPct = 0, linksPct = 0;
    if ((st.discovered||0) > 0) pagesPct = Math.min(100, Math.round((st.visited||0) * 100 / st.discovered));
    if ((st.total_links||0) > 0) linksPct = Math.min(100, Math.round((st.checked_links||0) * 100 / st.total_links));
    var combined = Math.round((pagesPct + linksPct) / 2);
    if ((st.state||"") !== "done") combined = Math.min(combined, 99); else combined = 100;
    $("#statVisited").textContent = "Visited: " + (st.visited||0);
    $("#statQueued").textContent = "Queued: " + (st.queued||0);
    $("#statDiscovered").textContent = "Discovered: " + (st.discovered||0);
    $("#statErrors").textContent = "Errors: " + (st.errors||0);
    const isDone = (st.state || "") === "done"
      || ((st.total_links||0) > 0 && (st.checked_links||0) >= st.total_links
          && (st.discovered||0) > 0 && (st.visited||0) >= st.discovered);
    document.querySelector("#progress").classList.toggle("done", isDone);
    document.querySelector("#barFill").style.width = (isDone ? "100" : String(combined)) + "%";
  }

  function classOf(r) {
    if (r.error && r.error.length) return "e";
    var c = r.status_code||0;
    if (c>=200 && c<300) return "2";
    if (c>=300 && c<400) return "3";
    if (c>=400 && c<500) return "4";
    if (c>=500 && c<600) return "5";
    return "e";
  }
  function matchesFilters(r) {
    if (MODE === 'internal' && !r.internal) return false;
    if (MODE === 'external' && r.internal) return false;
    if (MODE === '2' || MODE === '3' || MODE === '4' || MODE === '5' || MODE === 'e') {
      var cls = classOf(r);
      if (MODE !== cls) return false;
    }
    return true;
  }

  function renderTable() {
    var tbody = $("#table tbody");
    tbody.innerHTML = "";
    var total=0, internal=0, external=0, broken=0;
    var rows = (ALL_RESULTS || []).filter(matchesFilters);
    (ALL_RESULTS || []).forEach(function(r){
      total++;
      if (r.internal) internal++; else external++;
      var c = classOf(r);
      if (c==="4" || c==="5" || c==="e") broken++;
    });
    $("#summary").textContent = "Итого: " + total + " | Внутр.: " + internal + " | Внешн.: " + external + " | Нерабочих: " + broken;

    rows.forEach(function(r){
      var tr = document.createElement("tr");
      var cls = classOf(r);
      if (cls==="4" || cls==="5" || cls==="e") tr.className = "bad";
      else if (cls==="3") tr.className = "warn";
      else tr.className = "good";

      var urlTD = '<a href="' + escapeHtml(r.url) + '" target="_blank" rel="noopener noreferrer">' + escapeHtml(r.url) + '</a>';
      var pageTD = r.page_url ? ('<a href="' + escapeHtml(r.page_url) + '" target="_blank" rel="noopener noreferrer">' + escapeHtml(r.page_url) + '</a>') : "";
      var st = r.error ? "ERR" : (r.status_code || "");
      var ms = (r.elapsed_ms || "");

      tr.innerHTML = ''
        + '<td>' + urlTD + '</td>'
        + '<td>' + pageTD + '</td>'
        + '<td>' + st + '</td>'
        + '<td>' + ms + '</td>'
        + '<td>' + (r.internal ? "yes" : "no") + '</td>';
      tbody.appendChild(tr);
    });
  }

  function escapeHtml(s){
    if (s === undefined || s === null) s = "";
    s = String(s);
    var ENT = { "&":"&amp;", "<":"&lt;", ">":"&gt;", "\"":"&quot;", "'":"&#039;" };
    return s.replace(/[&<>\"']/g, function(c){ return ENT[c] || c; });
  }
})();