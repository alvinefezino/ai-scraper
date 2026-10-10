/* theme: persist + system preference */
(function(){
  const KEY="scrapeai-theme";
  function getPref(){
    try{ const s=localStorage.getItem(KEY); if(s==="light"||s==="dark") return s; }catch(_){}
    return matchMedia("(prefers-color-scheme: dark)").matches ? "dark":"light";
  }
  function apply(t){
    document.documentElement.setAttribute("data-theme", t);
    const icon = t==="dark"
      ? '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="5"/><path d="M12 1v2M12 21v2M4.2 4.2l1.4 1.4M18.4 18.4l1.4 1.4M1 12h2M21 12h2M4.2 19.8l1.4-1.4M18.4 5.6l1.4-1.4"/></svg>'
      : '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 12.79A9 9 0 1111.21 3 7 7 0 0021 12.79z"/></svg>';
    document.querySelectorAll("[data-theme-icon]").forEach(el=> el.innerHTML=icon);
  }
  window.__setTheme = function(next){
    const t = next || (document.documentElement.getAttribute("data-theme")==="dark" ? "light":"dark");
    try{ localStorage.setItem(KEY, t);}catch(_){}
    apply(t);
  };
  apply(getPref());
})();
function toggleDrawer(){
  const d=document.getElementById("mobileDrawer");
  if(d) d.classList.toggle("open");
}
function setPrompt(t){
  const el=document.getElementById('prompt');
  if(el){ el.value=t; el.focus(); }
}
function fillDemo(){
  const u=document.getElementById('url');
  const p=document.getElementById('prompt');
  if(u) u.value='https://books.toscrape.com/';
  if(p) p.value='Extract all books with title, price, rating and availability.';
  window.scrollTo({top:0, behavior:'smooth'});
  setTimeout(()=>{ if(u) u.focus(); }, 300);
}
function fillAnalyzeDemo(){
  const u=document.getElementById('url');
  if(u) u.value='https://linear.app/';
  window.scrollTo({top:0, behavior:'smooth'});
  setTimeout(()=>{ if(u) u.focus(); }, 300);
}
function switchTab(name){
  document.querySelectorAll('#resultTabs .tab').forEach(b=>{
    b.classList.toggle('active', b.dataset.tab===name);
  });
  document.querySelectorAll('.tab-panel').forEach(p=> p.classList.remove('active'));
  const panel=document.getElementById('panel-'+name);
  if(panel) panel.classList.add('active');
}
function toast(msg){
  const t=document.getElementById('toast');
  if(!t) return;
  t.textContent=msg;
  t.classList.add('show');
  clearTimeout(t._tm);
  t._tm=setTimeout(()=> t.classList.remove('show'), 2200);
}
async function copyJSON(id){
  const ta=document.getElementById('jsonData-'+id);
  if(!ta){ toast('Nothing to copy'); return; }
  try{ await navigator.clipboard.writeText(ta.value); toast('JSON copied to clipboard'); }
  catch{
    ta.style.display='block'; ta.select(); document.execCommand('copy'); ta.style.display='none';
    toast('JSON copied');
  }
}
async function copyText(text, msg){
  try{ await navigator.clipboard.writeText(text); toast(msg||'Copied'); }
  catch{
    const ta=document.createElement('textarea'); ta.value=text; ta.style.position='fixed'; ta.style.opacity='0';
    document.body.appendChild(ta); ta.select(); document.execCommand('copy'); ta.remove(); toast(msg||'Copied');
  }
}
async function copyColor(hex){ await copyText(hex, hex+' copied'); }
window.__scraperInit = function(){
  document.querySelectorAll('#resultTabs .tab').forEach(b=>{
    b.onclick = ()=> switchTab(b.dataset.tab);
  });
};
document.addEventListener('htmx:afterSwap', function(e){
  if(e.detail && e.detail.target && e.detail.target.id==='results'){
    if(window.__scraperInit) window.__scraperInit();
    const r=document.getElementById('results');
    if(r) r.scrollIntoView({behavior:'smooth', block:'start'});
  }
});
document.addEventListener('htmx:responseError', function(){ toast('Request failed. Check URL and try again'); });
