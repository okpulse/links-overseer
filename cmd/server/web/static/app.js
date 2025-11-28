(function(){
  const $ = s => document.querySelector(s);
  const $$ = s => Array.from(document.querySelectorAll(s));

  let JOB_ID = null;
  let ALL_RESULTS = [];
  let ALL_IMAGES = [];
  let STOPPED = false;
  let MODE = 'all';
  let ACTIVE_TAB = 'links';

  function setActiveTab(tab){
    ACTIVE_TAB = tab;
    $$(".tab").forEach(btn => {
      btn.classList.toggle("active", btn.getAttribute("data-tab") === tab);
    });
    const linksPanel = $("#linksPanel");
    const imagesPanel = $("#imagesPanel");
    if (linksPanel && imagesPanel){
      linksPanel.classList.toggle("hidden-panel", tab !== "links");
      imagesPanel.classList.toggle("hidden-panel", tab !== "images");
    }
    if (tab === "links") {
      renderTable();
    } else {
      renderImages();
    }
  }

  // tabs init
  $$("#mainTabs .tab").forEach(btn => {
    btn.addEventListener("click", function(){
      const tab = btn.getAttribute("data-tab");
      setActiveTab(tab);
    });
  });
  setActiveTab("links");

  $("#startBtn").onclick = async function () {
    STOPPED = false;
    const start_url = $("#startUrl").value.trim();
    var depth = parseInt($("#depth").value || "2", 10);
    if (isNaN(depth)) depth = 2;
    depth = Math.max(0, Math.min(5, depth));
    const respect_robots = $("#respectRobots").checked;
    const download_images = ($("#downloadImages") && $("#downloadImages").checked) || false;
    if (!/^https?:\/\/?/i.test(start_url)) { alert("Введите корректный URL, начиная с http(s)://"); return; }

    $("#progress").classList.remove("hidden");
    $("#progress").classList.remove("done");
    updateProgress({});

    try {
      const res = await fetch("/api/start", {
        method: "POST",
        headers: {"Content-Type":"application/json"},
        body: JSON.stringify({ start_url: start_url, depth: depth, respect_robots: respect_robots, download_images: download_images })
      });
      if (!res.ok) {
        const txt = await res.text();
        alert("Ошибка запуска: " + txt);
        return;
      }
      const payload = await res.json();
      JOB_ID = payload.job_id;
      ALL_RESULTS = [];
      ALL_IMAGES = [];
      renderTable();
      renderImages();
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

  function updateProgress(st){
    var pagesPct = 0, linksPct = 0;
    if ((st.discovered||0) > 0) pagesPct = Math.min(100, Math.round((st.visited||0) * 100 / st.discovered));
    if ((st.total_links||0) > 0) linksPct = Math.min(100, Math.round((st.checked_links||0) * 100 / st.total_links));
    var combined = Math.round((pagesPct + linksPct) / 2);
    if ((st.state||"") !== "done") combined = Math.min(combined, 99); else combined = 100;
    var visited = (st.visited||0);
    var queued = (st.queued||0);
    var discovered = (st.discovered||0);
    var errors = (st.errors||0);
    var statVisited = $("#statVisited");
    var statQueued = $("#statQueued");
    var statDiscovered = $("#statDiscovered");
    var statErrors = $("#statErrors");
    if (statVisited) statVisited.textContent = "Visited: " + visited;
    if (statQueued) statQueued.textContent = "Queued: " + queued;
    if (statDiscovered) statDiscovered.textContent = "Discovered: " + discovered;
    if (statErrors) statErrors.textContent = "Errors: " + errors;
    const isDone = (st.state || "") === "done"
      || ((st.total_links||0) > 0 && (st.checked_links||0) >= st.total_links
          && (st.discovered||0) > 0 && (st.visited||0) >= st.discovered);
    const prog = document.querySelector("#progress");
    const bar = document.querySelector("#barFill");
    if (prog) prog.classList.toggle("done", isDone);
    if (bar) bar.style.width = (isDone ? "100" : String(combined)) + "%";
  }

  function isJobFinished(st){
    if (!st) return false;
    var state = st.state || "";
    if (state === "done" || state === "failed" || state === "canceled") return true;
    var tl = st.total_links || 0;
    var cl = st.checked_links || 0;
    var disc = st.discovered || 0;
    var vis = st.visited || 0;
    if (tl > 0 && cl >= tl && disc > 0 && vis >= disc) return true;
    return false;
  }

  async function pollStatus() {
    if (!JOB_ID || STOPPED) return;
    try {
      const res = await fetch("/api/status?job=" + encodeURIComponent(JOB_ID));
      if (!res.ok) return;
      const st = await res.json();
      updateProgress(st);

      // обновляет ссылки и картинки
      try {
        const res2 = await fetch("/api/results?job=" + encodeURIComponent(JOB_ID));
        if (res2.ok) {
          ALL_RESULTS = await res2.json();
        }
      } catch(e) {}

      try {
        const res3 = await fetch("/api/images?job=" + encodeURIComponent(JOB_ID));
        if (res3.ok) {
          ALL_IMAGES = await res3.json();
        }
      } catch(e) {}

      if (ACTIVE_TAB === "links") {
        renderTable();
      } else {
        renderImages();
      }

      // стоп
      if (isJobFinished(st)) {
        STOPPED = true;
        return;
      }

      setTimeout(pollStatus, 800);
    } catch(e) {
      console.warn("pollStatus error:", e);
    }
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
    if (!tbody) return;
    tbody.innerHTML = "";
    var total=0, internal=0, external=0, broken=0;
    var rows = (ALL_RESULTS || []).filter(matchesFilters);
    (ALL_RESULTS || []).forEach(function(r){
      total++;
      if (r.internal) internal++; else external++;
      var c = classOf(r);
      if (c==="4" || c==="5" || c==="e") broken++;
    });
    var summary = $("#summary");
    if (summary) {
      summary.textContent = "Итого ссылок: " + total
        + " | Внутренних: " + internal
        + " | Внешних: " + external
        + " | Нерабочих/ошибок: " + broken;
    }

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

  function renderImages(){
    var tbody = $("#imagesTable tbody");
    if (!tbody) return;
    tbody.innerHTML = "";
    (ALL_IMAGES || []).forEach(function(img){
      var tr = document.createElement("tr");

      // превью
      var tdPrev = document.createElement("td");
      var thumb = document.createElement("img");
      thumb.className = "thumb";
      thumb.src = img.previewUrl || img.imageUrl || "";
      thumb.alt = img.alt || "";
      thumb.addEventListener("click", function(){
        openImageModal(thumb.src || img.imageUrl, img.alt || "");
      });
      tdPrev.appendChild(thumb);
      tr.appendChild(tdPrev);

      // URL картинки
      var tdUrl = document.createElement("td");
      var aImg = document.createElement("a");
      aImg.href = img.imageUrl || "";
      aImg.target = "_blank";
      aImg.rel = "noopener noreferrer";
      aImg.textContent = img.imageUrl || "";
      tdUrl.appendChild(aImg);
      tr.appendChild(tdUrl);

      // Страница
      var tdPage = document.createElement("td");
      if (img.pageUrl){
        var aPage = document.createElement("a");
        aPage.href = img.pageUrl;
        aPage.target = "_blank";
        aPage.rel = "noopener noreferrer";
        aPage.textContent = img.pageUrl;
        tdPage.appendChild(aPage);
      }
      tr.appendChild(tdPage);

      // ALT
      var tdAlt = document.createElement("td");
      tdAlt.textContent = img.alt || "";
      tr.appendChild(tdAlt);

      // Разрешение
      var tdRes = document.createElement("td");
      if (img.width && img.height){
        tdRes.textContent = img.width + "×" + img.height;
      } else {
        tdRes.textContent = "";
      }
      tr.appendChild(tdRes);

      // Метаданные
      var tdMeta = document.createElement("td");
      if (img.downloaded) {
        if (img.hasMetadata && img.metadataShort){
          tdMeta.textContent = img.metadataShort;
        } else {
          tdMeta.textContent = "метаданные не обнаружены";
        }
      } else {
        tdMeta.textContent = "Нажмите «Скачать» чтобы проверить";
        tdMeta.classList.add("muted");
      }
      tr.appendChild(tdMeta);

      // Действия: обратный поиск + скачать
      var tdAct = document.createElement("td");
      var actions = document.createElement("div");
      actions.className = "img-actions";

      var searchRow = document.createElement("div");
      searchRow.className = "search-row";

      var btnG = document.createElement("button");
      btnG.type = "button";
      btnG.textContent = "Google";
      btnG.onclick = function(){
        var u = img.imageUrl || "";
        if (!u) return;
        var q = "https://lens.google.com/uploadbyurl?url=" + encodeURIComponent(u);
        window.open(q, "_blank", "noopener");
      };
      searchRow.appendChild(btnG);

      var btnY = document.createElement("button");
      btnY.type = "button";
      btnY.textContent = "Yandex";
      btnY.onclick = function(){
        var u = img.imageUrl || "";
        if (!u) return;
        var q = "https://yandex.com/images/search?rpt=imageview&url=" + encodeURIComponent(u);
        window.open(q, "_blank", "noopener");
      };
      searchRow.appendChild(btnY);

      var btnB = document.createElement("button");
      btnB.type = "button";
      btnB.textContent = "Bing";
      btnB.onclick = function(){
        var u = img.imageUrl || "";
        if (!u) return;
        var q = "https://www.bing.com/images/search?view=detailv2&iss=sbi&FORM=SBIIDP&sbisrc=UrlPaste&idpbck=1&imgurl=" + encodeURIComponent(u);
        window.open(q, "_blank", "noopener");
      };
      searchRow.appendChild(btnB);

      actions.appendChild(searchRow);

      var btnDownload = document.createElement("button");
      btnDownload.type = "button";

      if (img.tooLarge) {
        btnDownload.disabled = true;
        btnDownload.textContent = "Слишком большой (>10 МБ)";
      } else {
        btnDownload.disabled = false;
        btnDownload.textContent = "Скачать";
      }

      btnDownload.onclick = async function () {
        if (!JOB_ID || img.tooLarge) return;
        btnDownload.disabled = true;
        btnDownload.textContent = "Загрузка...";
        try {
          const res = await fetch(
            "/api/images/download?job=" +
              encodeURIComponent(JOB_ID) +
              "&id=" +
              encodeURIComponent(img.id),
            { method: "POST" }
          );
          if (!res.ok) {
            alert("Ошибка скачивания картинки");
            btnDownload.disabled = false;
            btnDownload.textContent = "Скачать";
            return;
          }
          const updated = await res.json();
          ALL_IMAGES = (ALL_IMAGES || []).map(function (it) {
            return it.id === updated.id ? updated : it;
          });
          renderImages();
        } catch (e) {
          console.warn("download error", e);
          alert("Ошибка скачивания картинки");
          btnDownload.disabled = false;
          btnDownload.textContent = "Скачать";
        }
      };
      actions.appendChild(btnDownload);

      tdAct.appendChild(actions);
      tr.appendChild(tdAct);

      tbody.appendChild(tr);
    });
  }

  function openImageModal(src, alt){
    var modal = $("#imageModal");
    var imgEl = $("#imageModalImg");
    if (!modal || !imgEl) return;
    imgEl.src = src || "";
    imgEl.alt = alt || "";
    modal.classList.remove("hidden");
  }

  function closeImageModal(){
    var modal = $("#imageModal");
    var imgEl = $("#imageModalImg");
    if (modal) modal.classList.add("hidden");
    if (imgEl) imgEl.src = "";
  }

  var modalBackdrop = $("#imageModal .img-modal-backdrop");
  var modalClose = $("#imageModalClose");
  if (modalBackdrop){
    modalBackdrop.addEventListener("click", closeImageModal);
  }
  if (modalClose){
    modalClose.addEventListener("click", closeImageModal);
  }

  function escapeHtml(s){
    if (s === undefined || s === null) s = "";
    s = String(s);
    var ENT = { "&":"&amp;", "<":"&lt;", ">":"&gt;", "\"":"&quot;", "'":"&#039;" };
    return s.replace(/[&<>\"']/g, function(c){ return ENT[c] || c; });
  }
})();