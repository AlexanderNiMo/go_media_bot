// Browser regression checks against the Go fixture server, using only Node and Chrome.
import {spawn} from 'node:child_process';
import assert from 'node:assert/strict';
const [chromePath, origin, profile] = process.argv.slice(2);
const chrome = spawn(chromePath, ['--headless', '--disable-gpu', '--no-first-run',
  '--no-default-browser-check', '--remote-debugging-port=0', '--user-data-dir='+profile, 'about:blank']);
const timeout = setTimeout(() => { chrome.kill(); process.exitCode = 1; }, 60000);
let ws;
try {
  const endpoint = await new Promise((resolve, reject) => {
    chrome.stderr.on('data', b => { const m = b.toString().match(/DevTools listening on (ws:\/\/\S+)/); if (m) resolve(m[1]); });
    chrome.once('exit', code => reject(Error('Chrome exited: '+code)));
    chrome.once('error', reject);
  });
  const debugOrigin = endpoint.replace('ws:', 'http:').split('/devtools/')[0];
  const tabs = await (await fetch(debugOrigin+'/json/list')).json();
  const tab = tabs.find(t => t.type === 'page');
  ws = new WebSocket(tab.webSocketDebuggerUrl);
  await new Promise(r => ws.addEventListener('open', r, {once:true}));
  let seq = 0;
  const pending = new Map();
  ws.addEventListener('message', e => {
    const m = JSON.parse(e.data);
    if (!m.id) return;
    const p = pending.get(m.id); pending.delete(m.id);
    m.error ? p.reject(Error(JSON.stringify(m.error))) : p.resolve(m.result);
  });
  const send = (method, params={}) => new Promise((resolve,reject) => {
    const id = ++seq; pending.set(id,{resolve,reject}); ws.send(JSON.stringify({id,method,params}));
  });
  const evaluate = async expression => {
    const r = await send('Runtime.evaluate', {expression, returnByValue:true, awaitPromise:true});
    if (r.exceptionDetails) throw Error(JSON.stringify(r.exceptionDetails));
    return r.result.value;
  };
  const delay = ms => new Promise(r => setTimeout(r,ms));
  await send('Page.enable');
  await send('Network.enable');
  await send('Emulation.setDeviceMetricsOverride', {width:1440,height:900,deviceScaleFactor:1,mobile:false});
  await send('Page.navigate',{url:origin+'/mediad/'});
  for (let i=0; i<100; i++) {
    if (await evaluate(`document.readyState === 'complete' && typeof htmx !== 'undefined'`)) break;
    await delay(30);
  }
  await evaluate(`
    window.poll = document.getElementById('dashboard-poll');
    window.count = 0; window.requestTimes = []; window.failures = [];
    poll.addEventListener('htmx:beforeRequest', e => {
      if (!e.defaultPrevented) { count++; requestTimes.push(performance.now()); }
    });
    window.addEventListener('error', e => failures.push(e.message));
    window.refresh = () => htmx.ajax('GET', poll.getAttribute('hx-get'), {source:poll,target:poll,swap:'none settle:0'});
    window.titleLink = () => document.querySelector('[data-request="/mediad/requests/7"] .what a');
    window.pickLink = () => document.querySelector('[data-request="/mediad/requests/7"] .actions a:first-child');
    window.cancelLink = () => document.querySelector('[data-request="/mediad/requests/7"] .actions a:last-child');
  `);
  const check = async (expression, label) => assert.equal(await evaluate(expression), true, label);
  await check(`!document.querySelector('#dashboard-status, [aria-live], [role="status"]')`, 'dashboard has no live announcements');
  const stage = async n => { await fetch(origin+'/stage?n='+n); };
  const refresh = async n => { await stage(n); await evaluate('refresh()'); await delay(150); };

  for (const width of [320,769,1440]) {
    await send('Emulation.setDeviceMetricsOverride',{width,height:400,deviceScaleFactor:1,mobile:false});
    for (const theme of ['light','dark']) {
      await evaluate(`document.documentElement.dataset.theme='${theme}'`);
      for (const reverse of [false,true]) {
        await evaluate(`(() => {
          const target = titleLink();
          const controls = [...document.querySelectorAll('a[href],button,input,select,summary')]
            .filter(e => e.tabIndex >= 0 && !e.disabled && e.getClientRects().length && !e.closest('[inert]'));
          controls[controls.indexOf(target) + (${reverse} ? 1 : -1)].focus({preventScroll:true});
          const r = target.getBoundingClientRect();
          scrollTo(0, scrollY + r.top - (${reverse} ? -r.height/2 : innerHeight-r.height/2));
        })()`);
        for (const type of ['keyDown','keyUp']) {
          await send('Input.dispatchKeyEvent',{type,key:'Tab',code:'Tab',windowsVirtualKeyCode:9,modifiers:reverse?8:0});
        }
        await evaluate('new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))');
        await check(`(() => {
          const e = titleLink(), r = e.getBoundingClientRect(), s = getComputedStyle(e);
          const outline = parseFloat(s.outlineWidth) + parseFloat(s.outlineOffset);
          return document.activeElement === e && outline > 0 && r.top-outline >= 0 && r.bottom+outline <= innerHeight;
        })()`, 'Tab keeps the whole focus outline visible: '+width+' '+theme+' reverse='+reverse);
      }
    }
  }
  await send('Emulation.setDeviceMetricsOverride',{width:1440,height:900,deviceScaleFactor:1,mobile:false});
  await evaluate(`document.documentElement.removeAttribute('data-theme'); scrollTo(0,0)`);

  await evaluate(`window.original = titleLink(); original.focus(); window.originalBucket = original.closest('[id^="table-"]');`);
  await refresh(0);
  await check('titleLink() === original && original.closest("[id^=table-]") === originalBucket && document.activeElement === original', 'unchanged response keeps DOM and focus');
  await evaluate(`document.querySelector('[name=q]').value = 'не отправлять'; document.getElementById('block-searching').open = false; window.scrollTo(0, 150); window.scrollBefore = scrollY;`);
  await check('scrollBefore > 0', 'scroll check starts below the top');
  await refresh(1);
  await check('document.activeElement === titleLink() && scrollY === scrollBefore', 'progress replacement restores focus without scrolling');
  await check(`document.querySelector('[name=q]').value === 'не отправлять' && !document.getElementById('block-searching').open && poll.getAttribute('hx-get').includes('scope=mine') && poll.getAttribute('hx-get').includes('sort=waiting')`, 'filters, scope and disclosure survive');
  await send('Input.dispatchKeyEvent', {type:'keyDown',key:'Tab',code:'Tab',windowsVirtualKeyCode:9});
  await send('Input.dispatchKeyEvent', {type:'keyUp',key:'Tab',code:'Tab',windowsVirtualKeyCode:9});
  await check('document.activeElement === cancelLink()', 'Tab continues from restored title link');
  await refresh(2);
  await check('document.activeElement === cancelLink() && cancelLink().closest("details").id === "block-waiting"', 'cancel focus follows request to a later bucket');
  await evaluate(`
    window.pickBefore = pickLink(); pickBefore.focus();
    pickBefore.closest('tr').querySelector('[data-label="Возраст"]').textContent = 'до обновления';
  `);
  await refresh(2);
  await check('!pickBefore.isConnected && document.activeElement === pickLink()', 'pick action keeps focus after a changed table');
  await send('Input.dispatchKeyEvent', {type:'keyDown',key:'Tab',code:'Tab',windowsVirtualKeyCode:9});
  await send('Input.dispatchKeyEvent', {type:'keyUp',key:'Tab',code:'Tab',windowsVirtualKeyCode:9});
  await check('document.activeElement === cancelLink()', 'Tab continues from restored pick action to cancel');
  await evaluate('pickLink().focus()');
  await refresh(3);
  await check('document.activeElement === titleLink() && titleLink().closest("details").id === "block-downloading"', 'missing pick action falls back to title in an earlier bucket');
  await evaluate(`document.getElementById('block-waiting').open = false; titleLink().focus();`);
  await refresh(2);
  await check('document.activeElement === document.querySelector("#block-downloading summary") && !document.getElementById("block-waiting").open', 'closed destination keeps disclosure and focuses original summary');
  await evaluate(`document.getElementById('block-waiting').open = true; cancelLink().focus();`);
  await refresh(4);
  await check('document.activeElement === document.querySelector("#block-waiting summary")', 'removed request falls back to summary');
  await evaluate(`window.beforeError = document.getElementById('table-searching');`);
  await stage(5);
  await evaluate('refresh().catch(() => {})');
  await check('document.getElementById("table-searching") === beforeError', 'HTTP error leaves DOM alone');
  await refresh(0);
  await evaluate('window.count = 0; window.requestTimes = []');
  await delay(6500);
  const visibleRequests = await evaluate('count');
  assert.ok(visibleRequests >= 2 && visibleRequests <= 3, 'visible tab polls every 3 seconds');
  // A second real tab changes document.hidden, rather than mocking the property.
  const other = await send('Target.createTarget',{url:'about:blank'});
  await send('Target.activateTarget',{targetId:other.targetId});
  await delay(200);
  await check('document.hidden', 'real background tab is hidden');
  const hiddenStart = await evaluate('count');
  await delay(6500);
  assert.equal(await evaluate('count'), hiddenStart, 'no hidden-tab requests');
  await send('Target.activateTarget',{targetId:tab.id});
  await delay(3500);
  const resumeTimes = await evaluate('requestTimes.slice('+hiddenStart+')');
  assert.ok(resumeTimes.length >= 1 && resumeTimes.length <= 2, 'return resumes normal polling');
  const resumeIntervals = resumeTimes.slice(1).map((time, i) => time - resumeTimes[i]);
  assert.ok(resumeIntervals.every(ms => ms >= 2800), 'return does not burst queued polls');
  await send('Target.closeTarget',{targetId:other.targetId});
  await evaluate(`poll.setAttribute('hx-trigger', 'manual'); htmx.process(poll);`);
  await stage(6);
  await evaluate(`window.slowStart = count; window.busy = false; window.overlap = false;
    poll.addEventListener('htmx:beforeRequest', () => { if (busy) overlap = true; busy = true; });
    poll.addEventListener('htmx:afterRequest', () => { busy = false; });
    window.slow = refresh(); void 0;`);
  await delay(250);
  await evaluate(`document.querySelector('[name=q]').focus(); refresh(); refresh(); void 0;`);
  await delay(3900);
  await check(`!overlap && count === slowStart + 1 && document.activeElement === document.querySelector('[name=q]')`, 'slow poll does not queue or steal newly moved focus');
  await stage(0);
  const images = await evaluate(`performance.getEntriesByType('resource').filter(e=>e.name.includes('/poster/')).map(e=>({path:new URL(e.name).pathname,transfer:e.transferSize,bytes:e.decodedBodySize}))`);
  for (const width of [320,1440]) {
    await send('Emulation.setDeviceMetricsOverride',{width,height:900,deviceScaleFactor:1,mobile:false});
    for (const theme of ['light','dark']) {
      await evaluate(`document.documentElement.dataset.theme='${theme}'`);
      await check('document.documentElement.scrollWidth <= innerWidth', 'no horizontal overflow: '+width+' '+theme);
    }
  }
  await check('failures.length === 0','no JavaScript errors');
  console.log(JSON.stringify({checks:'passed',visibleRequestsIn6500ms:visibleRequests,hiddenRequestsIn6500ms:0,resumeRequestsIn3500ms:resumeTimes.length,resumeIntervalsMs:resumeIntervals,posters:images}));
} finally {
  clearTimeout(timeout);
  if (ws) ws.close();
  chrome.kill();
}
