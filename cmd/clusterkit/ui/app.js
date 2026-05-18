let config = null;
let selectedRepo = null;
let startupRoleAsked = false;
let generating = false;
let chatAbortController = null;
let contextMax = 131072;
let contextMaxKnown = false;
let chatMessages = [{role:'system', content:'You are a helpful local assistant.'}];
const $ = id => document.getElementById(id);
async function api(path, opts={}) { const r = await fetch(path, opts); if(!r.ok) throw new Error(await r.text()); return r.json(); }
function esc(s){return String(s||'').replaceAll('&','&amp;').replaceAll('"','&quot;').replaceAll('<','&lt;')}
function fmtBytes(n){ if(!n) return ''; const u=['B','KB','MB','GB']; let i=0; while(n>1024&&i<u.length-1){n/=1024;i++} return `${n.toFixed(i?1:0)} ${u[i]}` }
function fmtFileSize(n){ return fmtBytes(n) || 'size unknown'; }
function fmtGB(n){ return `${(n||0).toFixed(1)} GB`; }
function hasRole(){ return config && config.roleExplicit && (config.role === 'worker' || config.role === 'coordinator'); }
function updateContextValue(){
  const el=$('context');
  const value=Number(el?.value||0);
  if($('contextValue')) $('contextValue').textContent=String(value);
  if($('contextMaxLabel')) $('contextMaxLabel').textContent=contextMaxKnown?`max: ${contextMax}`:'max: unknown';
  if($('contextMaxBtn')) $('contextMaxBtn').disabled=!contextMaxKnown;
}
function setContextControl(value, max=0){
  const el=$('context'); if(!el) return;
  contextMaxKnown = Number(max)>0;
  contextMax = contextMaxKnown ? Number(max) : Math.max(131072, Number(value||0));
  el.max = String(contextMax);
  el.step = contextMax <= 8192 ? '128' : '256';
  el.value = String(Math.max(0, Math.min(Number(value||0), contextMax)));
  updateContextValue();
}
async function setContextMax(){
  if(!contextMaxKnown) return;
  const el=$('context'); if(!el) return;
  el.value=String(contextMax);
  updateContextValue();
  await save().catch(()=>{});
}
function updateGenerationControls(){
  if($('stopGenBtn')) $('stopGenBtn').disabled = !generating;
  if($('sendBtn')) $('sendBtn').disabled = generating;
  if($('sendBtnBottom')) $('sendBtnBottom').disabled = generating;
}
function stopGeneration(){
  if(chatAbortController) chatAbortController.abort();
}
function collect(){
  if(!config) config = {};
  config.llamaDir = $('llamaDir')?.value || config.llamaDir || '';
  config.modelsDir = $('modelsDirInput')?.value || config.modelsDir || '';
  config.modelPath = $('modelPath')?.value || config.modelPath || '';
  config.rpcPort = Number($('rpcPort')?.value || config.rpcPort || 50052);
  config.computeMode = $('computeMode')?.value || config.computeMode || 'auto';
  config.apiPort = Number($('apiPort')?.value || config.apiPort || 8080);
  config.context = Number($('context')?.value || config.context || 4096);
  config.gpuLayers = Number($('gpuLayers')?.value || config.gpuLayers || 20);
  config.threads = Number($('threads')?.value || config.threads || 8);
  config.parallel = Number($('parallel')?.value || config.parallel || 1);
  config.cacheRam = Number($('cacheRam')?.value || config.cacheRam || 0);
  config.batch = Number($('batch')?.value || config.batch || 512);
  config.uBatch = Number($('uBatch')?.value || config.uBatch || 512);
  config.tensorSplit = config.weightedMode ? '' : ($('tensorSplit')?.value || config.tensorSplit || '');
  config.workers = config.workers || [];
}
function renderRoleUI(){
  const role = config?.role;
  const gate = $('roleGate');
  const forcedGate = gate && !gate.classList.contains('hidden') && gate.dataset.force === '1';
  $('roleLabel').textContent = role || 'не вибрано';
  $('roleSubtitle').textContent = role === 'worker'
    ? 'Worker mode: цей компʼютер віддає ресурси coordinator-у.'
    : role === 'coordinator'
      ? 'Coordinator mode: цей компʼютер знаходить workers, запускає модель і API.'
      : 'Обери роль цього компʼютера.';
  $('workerUI').classList.toggle('hidden', role !== 'worker');
  $('coordinatorUI').classList.toggle('hidden', role !== 'coordinator');
  document.querySelectorAll('.coordinator-setting').forEach(el=>el.classList.toggle('hidden', role === 'worker'));
  if(!hasRole()) showRoleGate(false); else if(!forcedGate) hideRoleGate();
}
function showRoleGate(force=false){
  const gate=$('roleGate');
  gate.classList.remove('hidden');
  gate.dataset.force = force ? '1' : '0';
}
function hideRoleGate(){ $('roleGate').classList.add('hidden'); }
async function chooseRole(role){
  if(!config) config = {};
  config.role = role;
  config.roleExplicit = true;
  startupRoleAsked = true;
  await save();
  hideRoleGate();
  renderRoleUI();
}
function workerUsableGB(w){
  const ramGB=(w.ramBytes||0)/1024/1024/1024;
  const vramGB=(w.vramBytes||0)/1024/1024/1024;
  if(/cpu/i.test(w.backend||'')) return Math.max(2, Math.min(ramGB*0.55, ramGB-4));
  if(vramGB>0) return Math.max(0, vramGB*0.90);
  if(/darwin/i.test(w.os||'') && /arm64/i.test(w.arch||'')) return Math.max(0, Math.min(ramGB*0.65, ramGB-3));
  if(ramGB>0) return Math.max(2, ramGB*0.45);
  return 4;
}
function renderWorkers(workers=[]){
  const box = $('workers'); if(!box) return;
  box.innerHTML='';
  if(!workers.length){ box.innerHTML='<p class="muted">Workers поки не знайдені.</p>'; return; }
  workers.forEach(w=>{
    const div=document.createElement('div'); div.className='worker-card';
    const st = (w.status || (w.ok?'connected':'offline')).toLowerCase();
    const statusClass = st.includes('busy') ? 'warn' : (w.ok ? 'ok' : 'bad');
    const statusText = st.includes('busy') ? 'BUSY / AGENT ALIVE' : (w.ok ? 'CONNECTED' : 'OFFLINE');
    const seen = w.seenMs ? ` · heartbeat ${(w.seenMs/1000).toFixed(1)}s ago` : '';
    const status = `<span class="state ${statusClass}">${statusText}${seen}</span>`;
    const os = `${w.os||'unknown'}/${w.arch||'?'}`;
    const cpu = w.threads ? `${w.threads} CPU threads` : 'CPU threads ?';
    const ram = w.ramBytes ? fmtBytes(w.ramBytes) : '?';
    const vram = w.vramBytes ? fmtBytes(w.vramBytes) : '—';
    const backend = w.backend || 'unknown';
    const usable = fmtGB(workerUsableGB(w));
    div.innerHTML=`
      <strong>${esc(w.name||w.host)}</strong>
      <small>${esc(w.host)}:${w.port||50052}</small>
      <div class="worker-specs">
        <span>OS: <b>${esc(os)}</b></span>
        <span>CPU: <b>${esc(cpu)}</b></span>
        <span>RAM: <b>${esc(ram)}</b></span>
        <span>VRAM: <b>${esc(vram)}</b></span>
        <span>Backend: <b>${esc(backend)}</b></span>
        <span>Crashes: <b>${esc(w.crashCount||0)}</b></span>
        <span>Stability: <b>${esc(w.stability?Number(w.stability).toFixed(2):'1.00')}</b></span>
        <span>Process RAM: <b>${esc(w.rssBytes?fmtBytes(w.rssBytes):'—')}</b></span>
        <span>Model loaded: <b>${esc(w.loadPct?Number(w.loadPct).toFixed(0)+'%':'—')}</b></span>
        <span>Cluster usable: <b>${esc(usable)}</b></span>
        <span>App port: <b>${esc(w.appPort||'—')}</b></span>
      </div>
      ${status}`;
    box.appendChild(div);
  });
}
function fill(cfg){
  config=cfg || {};
  $('llamaDir').value=config.llamaDir||''; $('modelsDirInput').value=config.modelsDir||''; $('modelPath').value=config.modelPath||'';
  $('rpcPort').value=config.rpcPort||50052; if($('computeMode')) $('computeMode').value=config.computeMode||'auto'; $('apiPort').value=config.apiPort||8080; setContextControl(config.context||4096, config.modelMaxContext||0); $('gpuLayers').value=config.gpuLayers||20; $('threads').value=config.threads||8;
  $('parallel').value=config.parallel||1; $('cacheRam').value=config.cacheRam||0; $('batch').value=config.batch||512; $('uBatch').value=config.uBatch||512; if($('tensorSplit')) $('tensorSplit').value=config.tensorSplit||'';
  renderWorkers(config.workers||[]); refreshLocalModels(); renderChat(); renderRoleUI();
  if(!startupRoleAsked){ startupRoleAsked = true; showRoleGate(true); }
}
async function refresh(){
  const s=await api('/api/status'); if(!config) fill(s.config);
  window.lastStatus = s;
  $('localIP').textContent=s.localIP||'unknown'; $('os').textContent=`${s.os}/${s.arch}`;
  if($('computeMode')){ $('computeMode').disabled = s.os === 'darwin'; if(s.os === 'darwin') $('computeMode').value='gpu'; }
  $('modelsDir').textContent=s.modelsDir||''; const inst=s.installStatus||{}; $('llamaReady').textContent=s.llamaReady?`Ready (${inst.mode||'default'})`:`Not installed (${inst.reason||'missing'})`; $('llamaReady').className=s.llamaReady?'state ok':'state bad';
  if(s.hardware){ $('hardwareInfo').textContent=`${s.hardware.hostname} · ${s.hardware.cpuCount} CPU · ${fmtBytes(s.hardware.ramBytes)} RAM${s.hardware.vramBytes?' · '+fmtBytes(s.hardware.vramBytes)+' VRAM':''}${s.hardware.backend?' · '+s.hardware.backend:''}`; }
  if($('workerState')){ $('workerState').textContent=s.workerRunning?'Running':'Stopped'; $('workerState').className='state '+(s.workerRunning?'ok':'bad'); }
  if($('coordinatorState')){ $('coordinatorState').textContent=s.coordinatorRunning?'Running':'Stopped'; $('coordinatorState').className='state '+(s.coordinatorRunning?'ok':'bad'); }
  if($('loadState')){ const load=s.serverLoad||'idle'; const sec=s.loadStartedMs?Math.round(s.loadStartedMs/1000):0; $('loadState').textContent=s.coordinatorRunning?`Model: ${load}${load!=='ready'&&sec?` · ${sec}s`:''}`:'Model: idle'; }
  $('logs').textContent=(s.logs||[]).join('\n');
  if(config && config.role === 'coordinator'){
    config.workers = s.workerStatuses || s.config?.workers || [];
    renderWorkers(config.workers);
    renderCapacity(estimateCapacityFromStatus(s));
  }
  renderRoleUI();
}
async function save(){ collect(); await api('/api/save',{method:'POST',body:JSON.stringify(config)}); await refresh(); }
async function saveSettings(){ await save(); closeSettings(); }
function openSettings(){ $('settingsModal').classList.remove('hidden'); }
function closeSettings(){ $('settingsModal').classList.add('hidden'); }
async function installDeps(){ await save(); await api('/api/install',{method:'POST'}); alert('Install/check started. Якщо llama.cpp вже є — повторно качати не буде.'); }
async function forceRepairDeps(){ if(!confirm('Force repair заново скачає llama.cpp/CUDA runtime. Продовжити?')) return; await save(); await api('/api/install?force=1',{method:'POST'}); alert('Force repair started. Дивись прогрес у консолі.'); }
async function startWorker(){ await save(); await api('/api/start-worker',{method:'POST'}).catch(e=>alert(e.message)); await refresh(); }
async function stopWorker(){ await api('/api/stop-worker',{method:'POST'}); await refresh(); }
async function startCoordinator(){ await save(); await api('/api/start-coordinator',{method:'POST'}).catch(e=>alert(e.message)); await refresh(); }
async function stopCoordinator(){ await api('/api/stop-coordinator',{method:'POST'}); await refresh(); }
async function checkWorkers(){ const workers=await api('/api/check-workers',{method:'POST'}); config.workers=workers; renderWorkers(workers); }
async function discoverWorkers(){
  const peers=await api('/api/discover',{method:'POST'}).catch(e=>{alert(e.message); return []});
  config.workers = peers.map(p=>({name:p.name||p.host,host:p.host,port:p.port||50052,appPort:p.appPort||8765,os:p.os,arch:p.arch,ramBytes:p.ramBytes||p.RAM||0,vramBytes:p.vramBytes||0,backend:p.backend||'',threads:p.threads||0,crashCount:p.crashCount||0,stability:p.stability||1,rssBytes:p.rssBytes||0,loadPct:p.loadPct||0,status:'discovered'}));
  renderWorkers(config.workers); await save(); await checkWorkers(); alert(`Found ${peers.length} worker(s)`);
}
function estimateCapacity(){ renderCapacity(estimateCapacityFromStatus(window.lastStatus || null)); }
function estimateCapacityFromStatus(s){
  if(!s) return null;
  const macRamGB=(s.hardware?.ramBytes||0)/1024/1024/1024;
  const macUsable=Math.max(0, Math.min(macRamGB*0.72, macRamGB-3));
  const workers=(s.workerStatuses||s.config?.workers||[]).filter(w=>w.ok);
  const workerUsable=workers.reduce((sum,w)=>sum+workerUsableGB(w),0);
  const total=macUsable+workerUsable;
  const ctx = total>=48?16384:total>=28?8192:4096;
  const tiers=[
    {name:'7B Q4',gb:5.5},{name:'14B Q4',gb:10.5},{name:'32B Q4',gb:23},{name:'35B Q2/Q3',gb:15},{name:'35B Q4',gb:26},{name:'70B Q2',gb:32},{name:'70B Q4',gb:48}
  ];
  const ok=tiers.filter(t=>t.gb*1.25<total).map(t=>t.name);
  const tight=tiers.filter(t=>t.gb<total && t.gb*1.25>=total).map(t=>t.name);
  const warnings=[];
  if(workers.length===0) warnings.push('Workers не connected — оцінка тільки для Mac.');
  if(workers.some(w=>!w.backend || /cpu|unknown/i.test(w.backend||''))) warnings.push('Worker backend невідомий/CPU-only — приріст може бути малий.');
  if(total<18) warnings.push('Для 35B буде tight: тримай context 4096 і parallel 1.');
  return {macUsable,workerUsable,total,ctx,ok,tight,warnings,workers:workers.length};
}
function renderCapacity(c){
  const box=$('capacityBox'); if(!box || !c) return;
  box.innerHTML=`
    <div class="capacity-grid">
      <div><strong>${fmtGB(c.macUsable)}</strong><small>Mac usable estimate</small></div>
      <div><strong>${fmtGB(c.workerUsable)}</strong><small>Workers usable estimate (${c.workers})</small></div>
      <div><strong>${fmtGB(c.total)}</strong><small>Total usable estimate</small></div>
      <div><strong>${c.ctx}</strong><small>Recommended max context</small></div>
    </div>
    <div class="capacity-section"><strong>OK:</strong> ${c.ok.length?c.ok.map(esc).join(', '):'—'}</div>
    <div class="capacity-section"><strong>Tight/Risky:</strong> ${c.tight.length?c.tight.map(esc).join(', '):'—'}</div>
    ${c.warnings.length?`<div class="capacity-warn">${c.warnings.map(esc).join('<br>')}</div>`:''}
  `;
}
async function optimizeSettings(){ const rec=await api('/api/optimize',{method:'POST'}); setContextControl(rec.context, contextMaxKnown?contextMax:0); $('gpuLayers').value=rec.gpuLayers; $('threads').value=rec.threads; alert('Optimized: '+rec.reason); await refresh(); }
async function autoWeighted(){
  await save().catch(()=>{});
  const plan=await api('/api/auto-weighted',{method:'POST'}).catch(e=>{alert(e.message); return null});
  if(!plan) return;
  setContextControl(plan.context, contextMaxKnown?contextMax:0);
  $('gpuLayers').value=plan.gpuLayers;
  $('threads').value=plan.threads;
  $('parallel').value=plan.parallel;
  $('batch').value=plan.batch;
  $('uBatch').value=plan.uBatch;
  config.weightedMode = true;
  config.tensorSplit = '';
  if($('tensorSplit')) $('tensorSplit').value='';
  renderWeightedPlan(plan);
  await refresh();
}
function renderWeightedPlan(plan){
  const box=$('weightedPlanBox'); if(!box || !plan) return;
  const rows=(plan.workers||[]).map(w=>`<div class="worker-card"><strong>${esc(w.name||w.host)}</strong><small>${esc(w.host)} · ${esc(w.backend||'unknown')}</small><small>usable ${fmtGB(w.usableGB)} · weight ${Number(w.weight||0).toFixed(2)} · share ${Number(w.sharePct||0).toFixed(1)}%</small></div>`).join('');
  box.innerHTML=`<div class="capacity-section"><strong>Weighted plan:</strong> ctx ${plan.context}, batch ${plan.batch}, ubatch ${plan.uBatch}, parallel ${plan.parallel}<br><small>recommended split: ${esc(plan.tensorSplit||'auto')} · active mode: fit-first, no forced tensor-split</small><br><small>${esc(plan.reason||'')}</small></div><div class="worker-list">${rows}</div>`;
}
async function openAPI(){ await api('/api/open',{method:'POST'}); }
async function copyLogs(){
  const text=$('logs').textContent || '';
  try{ await navigator.clipboard.writeText(text); alert('Консоль скопійована'); }
  catch(e){ const ta=document.createElement('textarea'); ta.value=text; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); ta.remove(); alert('Консоль скопійована'); }
}
async function searchModels(){
  const q=encodeURIComponent($('modelQuery').value||'gguf'); $('hfResults').innerHTML='Searching...'; $('hfFiles').innerHTML='';
  const models=await api('/api/models/search?q='+q).catch(e=>{ $('hfResults').textContent=e.message; return [] });
  $('hfResults').innerHTML='';
  models.forEach(m=>{ const div=document.createElement('div'); div.className='item'; div.innerHTML=`<strong>${esc(m.id)}</strong><small> downloads: ${m.downloads||0} · likes: ${m.likes||0}</small><button onclick="loadFiles('${esc(m.id)}')">Файли</button>`; $('hfResults').appendChild(div); });
}
async function loadFiles(repo){
  selectedRepo=repo; $('hfFiles').innerHTML='Loading files...';
  const files=await api('/api/models/files?repo='+encodeURIComponent(repo)).catch(e=>{ $('hfFiles').textContent=e.message; return [] });
  $('hfFiles').innerHTML='';
  files.forEach(f=>{ const div=document.createElement('div'); div.className='item file-item'; const size=fmtFileSize(f.size); div.innerHTML=`<div class="file-title"><strong>${esc(f.rfilename)}</strong><span class="size-badge">${esc(size)}</span></div><small>GGUF model file · ${esc(size)}</small><button class="primary" onclick="downloadModel('${esc(repo)}','${esc(f.rfilename)}')">Download ${esc(size)}</button>`; $('hfFiles').appendChild(div); });
}
async function downloadModel(repo,file){ await api('/api/models/download',{method:'POST',body:JSON.stringify({repo,file})}); alert('Download started. Прогрес у консолі.'); }
async function clearLocalModelCache(){
  if(!confirm('Очистити локальний cache моделей на цьому девайсі?')) return;
  const keepSelected=$('keepSelectedOnClear')?.checked ?? true;
  await api('/api/models/cache-clear',{method:'POST',body:JSON.stringify({all:!keepSelected,keepSelected})}).catch(e=>{alert(e.message); throw e});
  await refreshLocalModels(); await refresh();
}
async function refreshLocalModels(){
  if(!$('localModels')) return;
  const items=await api('/api/models/local').catch(()=>[]); $('localModels').innerHTML='';
  const selected=items.find(m=>m.selected && m.maxContext);
  setContextControl(Number($('context')?.value || config?.context || 4096), selected?.maxContext || 0);
  items.forEach(m=>{ const div=document.createElement('div'); div.className='item'; const aux=m.aux?'· auxiliary mmproj/clip':''; const maxCtx=m.maxContext?`· max ctx: ${m.maxContext}`:''; const select=m.aux?'<button disabled>Aux file</button>':`<button onclick="selectModel('${esc(m.path)}')">Select</button>`; div.innerHTML=`<strong>${esc(m.name)}</strong><small>${fmtBytes(m.size)} ${m.selected?'· selected':''} ${maxCtx} ${aux}</small>${select}<button onclick="deleteModel('${esc(m.path)}')">Delete</button>`; $('localModels').appendChild(div); });
}
async function selectModel(path){ await api('/api/models/select',{method:'POST',body:JSON.stringify({path})}).catch(e=>{alert(e.message); throw e}); $('modelPath').value=path; await refreshLocalModels(); await refresh(); }
async function deleteModel(path){ if(!confirm('Delete model?')) return; await api('/api/models/delete',{method:'POST',body:JSON.stringify({path})}); await refreshLocalModels(); await refresh(); }
function renderChat(){
  const box=$('chatBox'); if(!box) return; box.innerHTML='';
  chatMessages.filter(m=>m.role!=='system').forEach(m=>{ const div=document.createElement('div'); div.className='msg '+m.role; const meta=m.meta?`<small>${esc(m.meta)}</small>`:''; div.innerHTML=`<strong>${m.role==='user'?'You':'Assistant'}</strong><div>${esc(m.content).replaceAll('\n','<br>')}</div>${meta}`; box.appendChild(div); });
  box.scrollTop=box.scrollHeight;
}
function clearChat(){ chatMessages=[{role:'system', content:'You are a helpful local assistant.'}]; renderChat(); }
function chatKey(e){ if(e.key==='Enter' && (e.metaKey||e.ctrlKey)){ sendChat(); } }
async function sendChat(){
  if(generating) return;
  const input=$('chatInput'); const text=input.value.trim(); if(!text) return; input.value='';
  chatMessages.push({role:'user', content:text}); renderChat();
  await generateAssistant();
}
async function regenerateLast(){
  if(generating) return;
  let lastUser=-1;
  for(let i=chatMessages.length-1;i>=0;i--){ if(chatMessages[i].role==='user'){ lastUser=i; break; } }
  if(lastUser < 0) return;
  chatMessages = chatMessages.slice(0, lastUser + 1);
  renderChat();
  await generateAssistant();
}
async function generateAssistant(){
  generating = true;
  chatAbortController = new AbortController();
  updateGenerationControls();
  const thinking={role:'assistant', content:'', meta:'generating…'}; chatMessages.push(thinking); renderChat();
  const started=performance.now();
  try{
    const res=await fetch('/api/chat-stream',{method:'POST',headers:{'Content-Type':'application/json'},signal:chatAbortController.signal,body:JSON.stringify({messages:chatMessages.filter(m=>m.content!=='' || m.role!=='assistant').map(({role,content})=>({role,content})),temperature:0.7})});
    if(!res.ok) throw new Error(await res.text());
    const reader=res.body.getReader(); const dec=new TextDecoder(); let buf='';
    while(true){
      const {done,value}=await reader.read();
      if(done) break;
      buf += dec.decode(value,{stream:true});
      const events=buf.split('\n\n'); buf=events.pop()||'';
      for(const ev of events){
        const lines=ev.split('\n'); const type=(lines.find(l=>l.startsWith('event:'))||'event: message').slice(6).trim(); const dataLine=lines.find(l=>l.startsWith('data:')); if(!dataLine) continue;
        const data=JSON.parse(dataLine.slice(5).trim());
        if(type==='token'){
          thinking.content += data.content || '';
          thinking.meta = `${((performance.now()-started)/1000).toFixed(1)}s · ${data.tokens||0} chunks`;
          renderChat();
        }else if(type==='status'){
          thinking.meta = `${data.message || 'Loading…'} ${((data.elapsedMs||0)/1000).toFixed(1)}s`;
          renderChat();
        }else if(type==='done'){
          const sec=(data.elapsedMs||0)/1000; const ft=data.firstTokenMs!==undefined?` · first ${(data.firstTokenMs/1000).toFixed(1)}s`:'';
          thinking.meta = `${sec.toFixed(1)}s · ${data.tokens||0} chunks${ft}`;
          renderChat();
        }else if(type==='error'){
          throw new Error(data.message||'stream error');
        }
      }
    }
    if(!thinking.content) thinking.content='';
  }catch(e){
    if(e.name === 'AbortError'){
      thinking.meta = 'stopped by user';
      if(!thinking.content) thinking.content = '';
    }else{
      thinking.content='Error: '+e.message;
    }
  }
  generating = false;
  chatAbortController = null;
  updateGenerationControls();
  renderChat();
}
refresh(); setInterval(refresh, 2000); setInterval(refreshLocalModels, 8000);
