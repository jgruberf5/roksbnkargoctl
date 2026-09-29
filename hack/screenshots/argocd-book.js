// Argo CD web UI captures for the book.
//
//   ARGOCD_URL=https://<hub>:30443 ARGOCD_SESSION=<jwt> \
//     node hack/screenshots/argocd-book.js <scene> <out-dir> [app] [namespace]
//
// Needs Puppeteer (npm install puppeteer). ARGOCD_SESSION is an Argo CD session
// token (POST /api/v1/session with a user that can read the Application); it is
// set as the UI's argocd.token cookie, so no password is typed into the page.
//
// Scenes:
//   syncing   the resource tree while a sync is running, and the sync status panel
//   healthy   application list, resource tree, sync waves panel, CNEInstance and
//             License summaries, the check-license hook's logs, Settings > Clusters
//             and Settings > Repositories
//   failed    the resource tree and sync status of a failed sync
//   deleting  the resource tree while the Application is being deleted
const puppeteer = require('puppeteer');
const fs = require('fs');
const path = require('path');

const [scene, outDir, app = 'bnk-bnkargo', appNS = 'argocd'] = process.argv.slice(2);
const base = (process.env.ARGOCD_URL || '').replace(/\/$/, '');
const token = process.env.ARGOCD_SESSION || '';
if (!scene || !outDir || !base || !token) {
  console.error('usage: ARGOCD_URL=… ARGOCD_SESSION=… node argocd-book.js <scene> <out-dir> [app] [namespace]');
  process.exit(2);
}
const sleep = ms => new Promise(r => setTimeout(r, ms));
const appURL = (q = '') => `${base}/applications/${appNS}/${app}${q}`;
const node = (group, kind, ns, name) => encodeURIComponent(`${group}/${kind}/${ns}/${name}/0`);

async function shot(p, url, file, { wait = 6000, selector } = {}) {
  await p.goto(url, { waitUntil: 'networkidle2', timeout: 60000 }).catch(() => {});
  if (selector) await p.waitForSelector(selector, { timeout: 20000 }).catch(() => {});
  await sleep(wait);
  // The tree view announces its switch to grouped nodes (over 15 pods) with a
  // tooltip that covers the graph. Hide it with CSS: removing the node React
  // owns crashed the UI ("Cannot read properties of null (reading
  // 'setAttribute')") on the next render.
  await p.evaluate(() => {
    for (const el of document.querySelectorAll('div')) {
      if (el.children.length === 0 && el.textContent.startsWith('Since the number of pods has surpassed')) {
        (el.closest('[role="tooltip"], .tippy-box, .tippy-popper') || el).style.visibility = 'hidden';
      }
    }
  }).catch(() => {});
  // Never keep a capture of Argo CD's crash page.
  if (await p.evaluate(() => document.body.innerText.startsWith('Something went wrong'))) {
    throw new Error(`${file}: the Argo CD UI crashed; not saving it`);
  }
  const out = path.join(outDir, file);
  await p.screenshot({ path: out });
  console.log('captured', out);
}

(async () => {
  fs.mkdirSync(outDir, { recursive: true });
  const b = await puppeteer.launch({
    headless: 'new', acceptInsecureCerts: true,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage', '--ignore-certificate-errors',
      '--disable-features=HttpsUpgrades,HttpsFirstBalancedMode,HttpsFirstModeV2'],
  });
  const p = await b.newPage();
  await p.setViewport({ width: 1600, height: 1000, deviceScaleFactor: 1.5 });
  const host = new URL(base).hostname;
  await p.setCookie({ name: 'argocd.token', value: token, domain: host, path: '/', secure: true, httpOnly: true });

  const tree = appURL('?view=tree&resource=');
  switch (scene) {
    case 'syncing':
      await shot(p, tree, 'sync-in-progress-tree.png', { wait: 4000 });
      await shot(p, appURL('?view=tree&operation=true'), 'sync-in-progress-status.png', { wait: 4000 });
      break;
    case 'healthy':
      await shot(p, `${base}/applications`, 'applications-list.png');
      await shot(p, tree, 'application-tree-healthy.png', { wait: 8000 });
      await shot(p, appURL('?view=tree&operation=true'), 'last-sync-waves.png');
      await shot(p, appURL(`?view=tree&node=${node('k8s.f5.com', 'CNEInstance', 'f5-bnk', 'f5-bnk-f5-cne-controller')}&tab=summary`), 'cneinstance-summary.png');
      await shot(p, appURL(`?view=tree&node=${node('batch', 'Job', 'roksbnkargoctl-check', 'check-license')}&tab=summary`), 'check-license-hook.png');
      await shot(p, appURL('?view=network&resource='), 'application-network.png', { wait: 8000 });
      await shot(p, appURL('?view=list&resource='), 'application-resource-list.png');
      await shot(p, `${base}/settings/clusters`, 'settings-clusters.png');
      await shot(p, `${base}/settings/repos`, 'settings-repositories.png');
      break;
    case 'failed':
      await shot(p, tree, 'sync-failed-tree.png');
      await shot(p, appURL('?view=tree&operation=true'), 'sync-failed-status.png');
      break;
    case 'deleting':
      await shot(p, tree, 'application-deleting.png', { wait: 3000 });
      break;
    default:
      console.error('unknown scene', scene);
      process.exit(2);
  }
  await b.close();
})().catch(e => { console.error('ERR', e.message.split('\n')[0]); process.exit(1); });
