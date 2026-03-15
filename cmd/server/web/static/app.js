(function(){
  const $ = s => document.querySelector(s);
  const $$ = s => Array.from(document.querySelectorAll(s));

  let JOB_ID = null;
  let ALL_RESULTS = [];
  let ALL_IMAGES = [];
  let ALL_DOCUMENTS = [];
  let STOPPED = false;
  let MODE = 'all';
  let DOC_MODE = 'all';
  let ACTIVE_TAB = 'links';
  let DOCUMENTS_VIEW = 'table';
  let DOCS_BULK_DOWNLOADING = false;
  let DOCS_BULK_ABORT = false;
  let CURRENT_START_URL = '';

  let WHOIS_LOADED = false;
  let WHOIS_LOADING = false;
  let WHOIS_DATA = null;
  let WHOIS_TARGET = null;

  function setActiveTab(tab){
    ACTIVE_TAB = tab;
    $$(".tab").forEach(btn => {
      btn.classList.toggle("active", btn.getAttribute("data-tab") === tab);
    });
    const linksPanel = $("#linksPanel");
    const imagesPanel = $("#imagesPanel");
    const documentsPanel = $("#documentsPanel");
    const whoisPanel = $("#whoisPanel");
    if (linksPanel && imagesPanel && documentsPanel && whoisPanel){
      linksPanel.classList.toggle("hidden-panel", tab !== "links");
      imagesPanel.classList.toggle("hidden-panel", tab !== "images");
      documentsPanel.classList.toggle("hidden-panel", tab !== "documents");
      whoisPanel.classList.toggle("hidden-panel", tab !== "whois");
    }
    if (tab === "links") {
      renderTable();
    } else if (tab === "images") {
      renderImages();
    } else if (tab === "documents") {
      renderDocuments();
    } else if (tab === "whois") {
      loadWhoisIfNeeded();
    }
  }

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
    CURRENT_START_URL = start_url;
    var depth = parseInt($("#depth").value || "2", 10);
    if (isNaN(depth)) depth = 2;
    depth = Math.max(0, Math.min(5, depth));
    const respect_robots = $("#respectRobots").checked;
    const download_images = ($("#downloadImages") && $("#downloadImages").checked) || false;
    const download_documents = ($("#downloadDocuments") && $("#downloadDocuments").checked) || false;
    if (!/^https?:\/\//i.test(start_url)) { alert("Введите корректный URL, начиная с http(s)://"); return; }

    $("#progress").classList.remove("hidden");
    $("#progress").classList.remove("done");
    updateProgress({});

    try {
      const res = await fetch("/api/start", {
        method: "POST",
        headers: {"Content-Type":"application/json"},
        body: JSON.stringify({ start_url, depth, respect_robots, download_images, download_documents })
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
      ALL_DOCUMENTS = [];
      renderTable();
      renderImages();
      renderDocuments();
      pollStatus();
    } catch(e) {
      console.warn("start error:", e);
    }
  };

  $("#stopBtn").onclick = async function(){
    DOCS_BULK_ABORT = true;
    if (!JOB_ID) return;
    try{ await fetch("/api/stop?job=" + encodeURIComponent(JOB_ID), {method:"POST"}); }catch(e){}
  };

  $("#filterChips").addEventListener("click", function(ev){
    const btn = ev.target.closest(".chip");
    if (!btn) return;
    $$("#filterChips .chip").forEach(b => b.classList.remove("active"));
    btn.classList.add("active");
    MODE = btn.getAttribute("data-mode");
    renderTable();
  });

  const docFilter = $("#documentFilterChips");
  if (docFilter) {
    docFilter.addEventListener("click", function(ev){
      const btn = ev.target.closest(".chip");
      if (!btn) return;
      $$("#documentFilterChips .chip").forEach(b => b.classList.remove("active"));
      btn.classList.add("active");
      DOC_MODE = btn.getAttribute("data-doc-mode") || "all";
      renderDocuments();
    });
  }

  const downloadAllDocumentsBtn = $("#downloadAllDocumentsBtn");
  const documentsReportBtn = $("#documentsReportBtn");
  const documentsTableBtn = $("#documentsTableBtn");
  const documentsExportPdfBtn = $("#documentsExportPdfBtn");
  const documentsExportJsonBtn = $("#documentsExportJsonBtn");

  if (downloadAllDocumentsBtn) {
    downloadAllDocumentsBtn.addEventListener("click", async function(){
      if (DOCS_BULK_DOWNLOADING) return;
      await downloadAllDocuments();
    });
  }
  if (documentsReportBtn) {
    documentsReportBtn.addEventListener("click", function(){
      DOCUMENTS_VIEW = "report";
      renderDocuments();
    });
  }
  if (documentsTableBtn) {
    documentsTableBtn.addEventListener("click", function(){
      DOCUMENTS_VIEW = "table";
      renderDocuments();
    });
  }
  if (documentsExportJsonBtn) {
    documentsExportJsonBtn.addEventListener("click", function(){
      exportDocumentsReportJSON();
    });
  }
  if (documentsExportPdfBtn) {
    documentsExportPdfBtn.addEventListener("click", function(){
      exportDocumentsReportPDF();
    });
  }

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

      try {
        const res2 = await fetch("/api/results?job=" + encodeURIComponent(JOB_ID));
        if (res2.ok) ALL_RESULTS = await res2.json();
      } catch(e) {}

      try {
        const res3 = await fetch("/api/images?job=" + encodeURIComponent(JOB_ID));
        if (res3.ok) ALL_IMAGES = await res3.json();
      } catch(e) {}

      try {
        const res4 = await fetch("/api/documents?job=" + encodeURIComponent(JOB_ID));
        if (res4.ok) ALL_DOCUMENTS = await res4.json();
      } catch(e) {}

      if (ACTIVE_TAB === "links") {
        renderTable();
      } else if (ACTIVE_TAB === "images") {
        renderImages();
      } else if (ACTIVE_TAB === "documents") {
        renderDocuments();
      }

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

      var tdPrev = document.createElement("td");
      var thumb = document.createElement("img");
      thumb.className = "thumb";
      thumb.src = img.previewUrl || img.imageUrl || "";
      thumb.alt = img.alt || "";
      thumb.addEventListener("click", function(){ openImageModal(thumb.src || img.imageUrl, img.alt || ""); });
      tdPrev.appendChild(thumb);
      tr.appendChild(tdPrev);

      var tdUrl = document.createElement("td");
      var aImg = document.createElement("a");
      aImg.href = img.imageUrl || "";
      aImg.target = "_blank";
      aImg.rel = "noopener noreferrer";
      aImg.textContent = img.imageUrl || "";
      tdUrl.appendChild(aImg);
      tr.appendChild(tdUrl);

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

      var tdAlt = document.createElement("td");
      tdAlt.textContent = img.alt || "";
      tr.appendChild(tdAlt);

      var tdRes = document.createElement("td");
      tdRes.textContent = (img.width && img.height) ? (img.width + "×" + img.height) : "";
      tr.appendChild(tdRes);

      var tdMeta = document.createElement("td");
      if (img.downloaded) {
        tdMeta.textContent = (img.hasMetadata && img.metadataShort) ? img.metadataShort : "метаданные не обнаружены";
      } else {
        tdMeta.textContent = "Нажмите «Скачать» чтобы проверить";
        tdMeta.classList.add("muted");
      }
      tr.appendChild(tdMeta);

      var tdAct = document.createElement("td");
      var actions = document.createElement("div");
      actions.className = "img-actions";

      var searchRow = document.createElement("div");
      searchRow.className = "search-row";
      [["Google", "https://lens.google.com/uploadbyurl?url="], ["Yandex", "https://yandex.com/images/search?rpt=imageview&url="], ["Bing", "https://www.bing.com/images/search?view=detailv2&iss=sbi&FORM=SBIIDP&sbisrc=UrlPaste&idpbck=1&imgurl="]].forEach(function(pair){
        var btn = document.createElement("button");
        btn.type = "button";
        btn.textContent = pair[0];
        btn.onclick = function(){
          var u = img.imageUrl || "";
          if (!u) return;
          window.open(pair[1] + encodeURIComponent(u), "_blank", "noopener");
        };
        searchRow.appendChild(btn);
      });
      actions.appendChild(searchRow);

      var btnDownload = document.createElement("button");
      btnDownload.type = "button";
      if (img.tooLarge) {
        btnDownload.disabled = true;
        btnDownload.textContent = "Слишком большой (>10 МБ)";
      } else {
        btnDownload.textContent = "Скачать";
      }
      btnDownload.onclick = async function () {
        if (!JOB_ID || img.tooLarge) return;
        btnDownload.disabled = true;
        btnDownload.textContent = "Загрузка...";
        try {
          const res = await fetch("/api/images/download?job=" + encodeURIComponent(JOB_ID) + "&id=" + encodeURIComponent(img.id), { method: "POST" });
          if (!res.ok) {
            alert("Ошибка скачивания картинки");
            btnDownload.disabled = false;
            btnDownload.textContent = "Скачать";
            return;
          }
          const updated = await res.json();
          ALL_IMAGES = (ALL_IMAGES || []).map(function (it) { return it.id === updated.id ? updated : it; });
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


  function documentCanBeDownloaded(doc){
    if (!doc || doc.tooLarge) return false;
    if (!JOB_ID) return false;
    if (!doc.downloaded) return true;
    if (doc.downloadError) return true;
    if ((doc.status || "") === "остановлен") return true;
    if (doc.downloaded && !doc.hasMetadata) return true;
    return false;
  }

  function updateDocumentsActionState(){
    var actions = $("#documentsActions");
    var downloadBtn = $("#downloadAllDocumentsBtn");
    var reportBtn = $("#documentsReportBtn");
    var tableBtn = $("#documentsTableBtn");
    var filter = $("#documentFilterChips");
    var tableWrap = $("#documentsTableWrapper");
    var exportPdfBtn = $("#documentsExportPdfBtn");
    var exportJsonBtn = $("#documentsExportJsonBtn");
    var reportWrap = $("#documentsReportWrapper");
    if (actions) actions.classList.toggle("hidden-panel", ACTIVE_TAB !== "documents");
    if (filter) filter.classList.toggle("hidden-panel", DOCUMENTS_VIEW !== "table");
    if (tableWrap) tableWrap.classList.toggle("hidden-panel", DOCUMENTS_VIEW !== "table");
    if (reportWrap) reportWrap.classList.toggle("hidden-panel", DOCUMENTS_VIEW !== "report");
    if (tableBtn) tableBtn.classList.toggle("hidden-panel", DOCUMENTS_VIEW !== "report");
    if (reportBtn) reportBtn.classList.toggle("hidden-panel", DOCUMENTS_VIEW === "report");
    if (exportPdfBtn) exportPdfBtn.disabled = !(ALL_DOCUMENTS || []).some(function(doc){ return !!doc.downloaded; });
    if (exportJsonBtn) exportJsonBtn.disabled = !(ALL_DOCUMENTS || []).some(function(doc){ return !!doc.downloaded; });
    if (downloadBtn) {
      var pending = (ALL_DOCUMENTS || []).filter(documentCanBeDownloaded).length;
      downloadBtn.disabled = DOCS_BULK_DOWNLOADING || pending === 0;
      downloadBtn.textContent = DOCS_BULK_DOWNLOADING ? "Скачивание..." : "Скачать все";
      downloadBtn.title = pending === 0 ? "Нет документов для скачивания" : "";
    }
  }

  async function downloadSingleDocument(docId){
    const res = await fetch("/api/documents/download?job=" + encodeURIComponent(JOB_ID) + "&id=" + encodeURIComponent(docId), { method: "POST" });
    if (!res.ok) {
      const txt = await res.text();
      throw new Error(txt || "download failed");
    }
    return await res.json();
  }

  async function refreshDocumentsFromServer(){
    if (!JOB_ID) return [];
    const res = await fetch("/api/documents?job=" + encodeURIComponent(JOB_ID));
    if (!res.ok) throw new Error("documents refresh failed");
    const docs = await res.json();
    ALL_DOCUMENTS = Array.isArray(docs) ? docs : [];
    return ALL_DOCUMENTS;
  }

  async function downloadAllDocuments(){
    if (!JOB_ID) return;
    DOCS_BULK_DOWNLOADING = true;
    DOCS_BULK_ABORT = false;
    try {
      try {
        await refreshDocumentsFromServer();
      } catch (e) {}
      renderDocuments();
      var ids = (ALL_DOCUMENTS || [])
        .filter(documentCanBeDownloaded)
        .map(function(doc){ return Number(doc.id); })
        .filter(function(id){ return Number.isFinite(id) && id > 0; });
      for (const docId of ids) {
        if (DOCS_BULK_ABORT) break;
        try {
          const updated = await downloadSingleDocument(docId);
          var replaced = false;
          ALL_DOCUMENTS = (ALL_DOCUMENTS || []).map(function(it){
            if (it.id === updated.id) {
              replaced = true;
              return updated;
            }
            return it;
          });
          if (!replaced) {
            try { await refreshDocumentsFromServer(); } catch (e) {}
          }
        } catch (e) {
          var msg = ((e && e.message) || "").toLowerCase();
          if (DOCS_BULK_ABORT || /canceled|cancelled|context canceled|остановлен/i.test(msg)) {
            break;
          }
          if (msg.indexOf("document not found") !== -1 || msg.indexOf("bad request") !== -1) {
            try { await refreshDocumentsFromServer(); } catch (e2) {}
          }
        }
        renderDocuments();
      }
    } finally {
      DOCS_BULK_DOWNLOADING = false;
      DOCS_BULK_ABORT = false;
      try { await refreshDocumentsFromServer(); } catch (e) {}
      renderDocuments();
    }
  }

  function splitMetadataParts(text){
    return String(text || "")
      .split("|")
      .map(function(v){ return v.replace(/^\s*\|+\s*/, "").trim(); })
      .filter(Boolean);
  }

  function collectDocMetadataEntries(doc){
    var entries = [];
    function add(key, value){
      key = String(key || "").trim();
      value = String(value || "").trim();
      if (!key || !value) return;
      entries.push({key:key, value:value});
    }
    [
      ["Title", doc.title],
      ["Author", doc.author],
      ["Company", doc.company],
      ["Creator", doc.creator],
      ["Producer", doc.producer],
      ["LastModifiedBy", doc.lastModifiedBy],
      ["Created", doc.created],
      ["Modified", doc.modified],
    ].forEach(function(pair){ if (pair[1]) add(pair[0], pair[1]); });
    if (doc.contacts) {
      String(doc.contacts).split(",").map(function(v){ return v.trim(); }).filter(Boolean).forEach(function(v){ add("Contacts", v); });
    }
    var known = {};
    entries.forEach(function(item){ known[(item.key + " " + item.value).toLowerCase()] = true; });
    splitMetadataParts(doc.metadataRawSummary || doc.metadataShort || "").forEach(function(part){
      var idx = part.indexOf(":");
      if (idx <= 0) return;
      var key = part.slice(0, idx).trim().replace(/^\|+\s*/, "");
      var value = part.slice(idx + 1).trim();
      if (!key || !value) return;
      if (/^count$/i.test(key)) return;
      var storeKey = key;
      if (/^app$/i.test(key)) storeKey = "App";
      var knownKeys = {title:1, author:1, company:1, creator:1, producer:1, lastmodifiedby:1, created:1, modified:1, contacts:1, app:1};
      if (!knownKeys[String(storeKey).toLowerCase()]) {
        value = key + ': ' + value;
        storeKey = 'Другое';
      }
      var sig = (storeKey + " " + value).toLowerCase();
      if (!known[sig]) {
        known[sig] = true;
        add(storeKey, value);
      }
    });
    return entries;
  }

  function formatReportDate(date){
    var d = date instanceof Date ? date : new Date(date || Date.now());
    var dd = String(d.getDate()).padStart(2, '0');
    var mm = String(d.getMonth() + 1).padStart(2, '0');
    var yyyy = String(d.getFullYear());
    var hh = String(d.getHours()).padStart(2, '0');
    var min = String(d.getMinutes()).padStart(2, '0');
    return dd + '.' + mm + '.' + yyyy + ' ' + hh + ':' + min;
  }

  function getDocumentsReportExportData(){
    var report = buildDocumentsReportData();
    var generatedAt = new Date();
    return {
      title: 'Отчёт изучения метаданных',
      site: CURRENT_START_URL || (($('#startUrl') && $('#startUrl').value) || ''),
      createdAt: generatedAt.toISOString(),
      createdAtDisplay: formatReportDate(generatedAt),
      summary: {
        downloadedDocuments: report.totals.downloaded,
        withMetadata: report.totals.withMetadata,
        sectionsCount: report.sections.length,
        valuesCount: report.totals.values
      },
      categories: report.sections.map(function(section){
        return {
          key: section.key,
          total: section.total,
          values: section.values.map(function(item){
            return { value: item.value, count: item.count };
          })
        };
      })
    };
  }

  function downloadBlobFile(blob, fileName){
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url;
    a.download = fileName;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(function(){ URL.revokeObjectURL(url); }, 1000);
  }

  function sanitizeExportFilePart(value){
    return String(value || 'report')
      .replace(/^https?:\/\//i, '')
      .replace(/[^a-zA-Z0-9а-яА-ЯёЁ._-]+/g, '_')
      .replace(/^_+|_+$/g, '')
      .slice(0, 60) || 'report';
  }

  function exportDocumentsReportJSON(){
    var data = getDocumentsReportExportData();
    if (!data.summary.downloadedDocuments) {
      alert('Нет скачанных документов для экспорта.');
      return;
    }
    var blob = new Blob([JSON.stringify(data, null, 2)], {type:'application/json;charset=utf-8'});
    downloadBlobFile(blob, 'metadata-report-' + sanitizeExportFilePart(data.site) + '.json');
  }

  function exportDocumentsReportPDF(){
    var data = getDocumentsReportExportData();
    if (!data.summary.downloadedDocuments) {
      alert('Нет скачанных документов для экспорта.');
      return;
    }
    if (!JOB_ID) {
      alert('Не найдена активная задача.');
      return;
    }
    var a = document.createElement('a');
    a.href = '/api/documents/report.pdf?job=' + encodeURIComponent(JOB_ID);
    a.download = '';
    document.body.appendChild(a);
    a.click();
    a.remove();
  }

  function buildDocumentsReportData(){
    var downloadedDocs = (ALL_DOCUMENTS || []).filter(function(doc){ return !!doc.downloaded; });
    var groups = {};
    var totals = {downloaded: downloadedDocs.length, withMetadata: 0, values: 0};
    downloadedDocs.forEach(function(doc){
      var entries = collectDocMetadataEntries(doc);
      if (entries.length) totals.withMetadata += 1;
      entries.forEach(function(entry){
        var key = entry.key || "Другое";
        if (!groups[key]) groups[key] = {key:key, total:0, values:{}};
        groups[key].total += 1;
        totals.values += 1;
        var valKey = entry.value;
        if (!groups[key].values[valKey]) groups[key].values[valKey] = {value:entry.value, count:0, docs:[]};
        groups[key].values[valKey].count += 1;
        groups[key].values[valKey].docs.push(doc);
      });
    });
    var sections = Object.keys(groups).sort(function(a,b){ return a.localeCompare(b, 'ru'); }).map(function(key){
      var values = Object.values(groups[key].values).sort(function(a,b){
        if (b.count !== a.count) return b.count - a.count;
        return a.value.localeCompare(b.value, 'ru');
      });
      return { key:key, total:groups[key].total, values:values };
    });
    return { totals:totals, sections:sections };
  }

  function renderDocumentsReport(){
    var wrap = $("#documentsReportWrapper");
    if (!wrap) return;
    var report = buildDocumentsReportData();
    wrap.innerHTML = "";
    var summary = document.createElement("div");
    summary.className = "docs-report-summary";
    if (!report.totals.downloaded) {
      summary.innerHTML = '<div class="docs-report-empty">Отчёт строится только по уже скачанным документам. Пока данных нет.</div>';
      wrap.appendChild(summary);
      return;
    }
    var cards = [
      ["Скачано документов", report.totals.downloaded],
      ["С метаданными", report.totals.withMetadata],
      ["Разделов метаданных", report.sections.length],
      ["Всего найденных значений", report.totals.values],
    ];
    cards.forEach(function(card){
      var el = document.createElement("div");
      el.className = "docs-report-card";
      el.innerHTML = '<div class="docs-report-card-value">' + escapeHtml(card[1]) + '</div><div class="docs-report-card-label">' + escapeHtml(card[0]) + '</div>';
      summary.appendChild(el);
    });
    wrap.appendChild(summary);
    report.sections.forEach(function(section){
      var block = document.createElement("section");
      block.className = "docs-report-section";
      var h = document.createElement("h3");
      h.textContent = section.key + ' (' + section.total + ')';
      block.appendChild(h);
      section.values.forEach(function(item){
        var details = document.createElement("details");
        details.className = "docs-report-details";
        var sum = document.createElement("summary");
        sum.innerHTML = '<span class="docs-report-value">' + escapeHtml(item.value) + '</span><span class="docs-report-badge">' + escapeHtml(item.count) + '</span>';
        details.appendChild(sum);
        var list = document.createElement("ul");
        list.className = "docs-report-doc-list";
        item.docs.forEach(function(doc){
          var li = document.createElement("li");
          var fileLink = document.createElement("a");
          fileLink.href = doc.fileUrl || "#";
          fileLink.target = "_blank";
          fileLink.rel = "noopener noreferrer";
          fileLink.textContent = (doc.fileName || doc.fileUrl || "Документ") + ' [' + (doc.fileType || '').toUpperCase() + ']';
          li.appendChild(fileLink);
          if (doc.pageUrl) {
            var span = document.createElement("span");
            span.className = "docs-report-page";
            span.textContent = ' — найдено на: ';
            li.appendChild(span);
            var pageLink = document.createElement("a");
            pageLink.href = doc.pageUrl;
            pageLink.target = "_blank";
            pageLink.rel = "noopener noreferrer";
            pageLink.textContent = doc.pageUrl;
            li.appendChild(pageLink);
          }
          list.appendChild(li);
        });
        details.appendChild(list);
        block.appendChild(details);
      });
      wrap.appendChild(block);
    });
  }
  function documentMatchesFilter(doc){
    if (DOC_MODE === "all") return true;
    return (doc.typeGroup || "") === DOC_MODE;
  }

  function formatBytes(bytes){
    bytes = Number(bytes || 0);
    if (!bytes) return "";
    if (bytes < 1024) return bytes + " B";
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + " KB";
    return (bytes / (1024 * 1024)).toFixed(2) + " MB";
  }

  function renderDocuments(){
    var docs = (ALL_DOCUMENTS || []);
    var summary = $("#documentsSummary");
    if (summary) {
      var downloaded = docs.filter(d => d.downloaded).length;
      var withMeta = docs.filter(d => d.hasMetadata).length;
      summary.textContent = "Найдено документов: " + docs.length + " | Скачано: " + downloaded + " | С метаданными: " + withMeta;
    }
    updateDocumentsActionState();
    if (DOCUMENTS_VIEW === "report") {
      renderDocumentsReport();
      return;
    }
    var tbody = $("#documentsTable tbody");
    if (!tbody) return;
    tbody.innerHTML = "";
    var rows = docs.filter(documentMatchesFilter);

    rows.forEach(function(doc){
      var tr = document.createElement("tr");
      if (doc.downloadError) tr.className = "bad";
      else if (doc.hasMetadata) tr.className = "good";

      var tdName = document.createElement("td");
      tdName.textContent = doc.fileName || "";
      tr.appendChild(tdName);

      var tdType = document.createElement("td");
      tdType.textContent = (doc.fileType || "").toUpperCase();
      tr.appendChild(tdType);

      var tdUrl = document.createElement("td");
      var aFile = document.createElement("a");
      aFile.href = doc.fileUrl || "";
      aFile.target = "_blank";
      aFile.rel = "noopener noreferrer";
      aFile.textContent = doc.fileUrl || "";
      tdUrl.appendChild(aFile);
      tr.appendChild(tdUrl);

      var tdPage = document.createElement("td");
      if (doc.pageUrl) {
        var aPage = document.createElement("a");
        aPage.href = doc.pageUrl;
        aPage.target = "_blank";
        aPage.rel = "noopener noreferrer";
        aPage.textContent = doc.pageUrl;
        tdPage.appendChild(aPage);
      }
      tr.appendChild(tdPage);

      var tdSize = document.createElement("td");
      tdSize.textContent = formatBytes(doc.sizeBytes);
      tr.appendChild(tdSize);

      var tdMeta = document.createElement("td");
      var lines = [];
      if (doc.status) lines.push(doc.status);
      if (doc.metadataShort) lines.push(doc.metadataShort);
      if (doc.downloadError) lines.push(doc.downloadError);
      tdMeta.textContent = lines.join(" | ") || "Нажмите «Скачать» чтобы проверить";
      if (!doc.downloaded && !doc.downloadError) tdMeta.classList.add("muted");
      tr.appendChild(tdMeta);

      var tdAct = document.createElement("td");
      var btn = document.createElement("button");
      btn.type = "button";
      if (doc.tooLarge) {
        btn.disabled = true;
        btn.textContent = "Слишком большой (>25 МБ)";
      } else if (DOCS_BULK_DOWNLOADING) {
        btn.disabled = true;
        btn.textContent = "Скачать";
      } else {
        btn.textContent = "Скачать";
      }
      btn.onclick = async function(){
        if (!JOB_ID || doc.tooLarge || DOCS_BULK_DOWNLOADING) return;
        btn.disabled = true;
        btn.textContent = "Загрузка...";
        try {
          const updated = await downloadSingleDocument(doc.id);
          ALL_DOCUMENTS = (ALL_DOCUMENTS || []).map(function(it){ return it.id === updated.id ? updated : it; });
          renderDocuments();
        } catch(e) {
          console.warn("document download error", e);
          alert("Ошибка скачивания документа: " + (((e && e.message) || "")));
          btn.disabled = false;
          btn.textContent = "Скачать";
        }
      };
      tdAct.appendChild(btn);
      tr.appendChild(tdAct);
      tbody.appendChild(tr);
    });
    updateDocumentsActionState();
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
  if (modalBackdrop) modalBackdrop.addEventListener("click", closeImageModal);
  if (modalClose) modalClose.addEventListener("click", closeImageModal);

  function getWhoisTarget(){
    const input = $("#startUrl");
    if (!input) return "";
    return (input.value || "").trim();
  }

  function loadWhoisIfNeeded(){
    const target = getWhoisTarget();
    const errorEl = $("#whoisError");
    const loadingEl = $("#whoisLoading");
    const summaryWrapper = $("#whoisSummaryWrapper");
    const rawWrapper = $("#whoisRawWrapper");

    if (!target) {
      if (loadingEl) loadingEl.classList.add("hidden");
      if (summaryWrapper) summaryWrapper.classList.add("hidden");
      if (rawWrapper) rawWrapper.classList.add("hidden");
      if (errorEl) {
        errorEl.textContent = "Введите URL или домен в поле выше, чтобы получить WHOIS.";
        errorEl.classList.remove("hidden");
      }
      WHOIS_LOADED = false;
      WHOIS_DATA = null;
      WHOIS_TARGET = null;
      return;
    }

    if (WHOIS_LOADED && WHOIS_DATA && WHOIS_TARGET === target) {
      renderWhois(WHOIS_DATA);
      return;
    }
    if (WHOIS_LOADING) return;

    WHOIS_LOADED = false;
    WHOIS_DATA = null;
    WHOIS_TARGET = null;
    loadWhois(target);
  }

  async function loadWhois(target){
    const loadingEl = $("#whoisLoading");
    const errorEl = $("#whoisError");
    const summaryWrapper = $("#whoisSummaryWrapper");
    const rawWrapper = $("#whoisRawWrapper");

    WHOIS_LOADING = true;
    if (errorEl) errorEl.classList.add("hidden");
    if (summaryWrapper) summaryWrapper.classList.add("hidden");
    if (rawWrapper) rawWrapper.classList.add("hidden");
    if (loadingEl) loadingEl.classList.remove("hidden");

    try {
      const res = await fetch("/api/whois?target=" + encodeURIComponent(target));
      if (!res.ok) {
        let txt = "";
        try { txt = await res.text(); } catch(e){}
        throw new Error(txt || ("HTTP " + res.status));
      }
      const data = await res.json();
      WHOIS_DATA = data;
      WHOIS_TARGET = target;
      WHOIS_LOADED = true;
      renderWhois(data);
    } catch (e) {
      WHOIS_LOADED = false;
      WHOIS_DATA = null;
      WHOIS_TARGET = null;
      if (errorEl) {
        const msg = (e && e.message) ? e.message : "ошибка запроса";
        errorEl.textContent = "Не удалось получить WHOIS: " + msg;
        errorEl.classList.remove("hidden");
      }
    } finally {
      WHOIS_LOADING = false;
      if (loadingEl) loadingEl.classList.add("hidden");
    }
  }

  function renderWhois(data){
    const summaryWrapper = $("#whoisSummaryWrapper");
    const rawWrapper = $("#whoisRawWrapper");
    const tbody = $("#whoisSummaryTable tbody");
    const rawEl = $("#whoisRaw");
    if (!summaryWrapper || !rawWrapper || !tbody || !rawEl) return;

    tbody.innerHTML = "";
    function addRow(label, value){
      if (value === undefined || value === null) return;
      if (Array.isArray(value) && value.length === 0) return;
      const tr = document.createElement("tr");
      const tdKey = document.createElement("td");
      tdKey.textContent = label;
      const tdVal = document.createElement("td");
      tdVal.textContent = Array.isArray(value) ? value.join(", ") : value;
      tr.appendChild(tdKey);
      tr.appendChild(tdVal);
      tbody.appendChild(tr);
    }

    addRow("Домен", data && data.domain || "");
    addRow("Регистратор", data && data.registrar || "");
    addRow("Владелец", data && data.registrant || "");
    addRow("Создан", data && data.creation_date || "");
    addRow("Обновлён", data && data.updated_date || "");
    addRow("Истекает", data && data.expiration_date || "");
    if (data && Array.isArray(data.name_servers) && data.name_servers.length) addRow("Name servers", data.name_servers);
    if (data && Array.isArray(data.emails) && data.emails.length) addRow("E-mail", data.emails);

    summaryWrapper.classList.remove("hidden");
    rawEl.textContent = (data && data.raw) ? data.raw : "";
    rawWrapper.classList.remove("hidden");
  }

  function escapeHtml(s){
    if (s === undefined || s === null) s = "";
    s = String(s);
    var ENT = { "&":"&amp;", "<":"&lt;", ">":"&gt;", '"':"&quot;", "'":"&#039;" };
    return s.replace(/[&<>"']/g, function(c){ return ENT[c] || c; });
  }
})();
