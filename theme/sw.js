/* Meow Music Service Worker
 *
 * 只做三件事，不碰音频：
 *   1. 预缓存壳（首页 HTML、图标、manifest），断网时也能打开界面；
 *   2. 导航请求用「网络优先、失败回退缓存」—— 发版立刻生效，不会被旧缓存卡住；
 *   3. 图标/manifest 这类不变资源用缓存优先。
 *
 * 明确不缓存：
 *   /api/           动态数据（歌词、历史、音源状态）
 *   /stream_live    ffmpeg 实时转码流，缓存了会把磁盘撑爆
 *   /stream_pcm     同上
 *   /url/           音频代理
 * 这些一律直连网络，缓存层不介入（Range 请求也不能被拦截）。
 */

const VERSION = 'meowmusic-v1';
const SHELL_CACHE = VERSION + '-shell';

// 只预缓存确定存在的静态资源；任何一个 404 都会让 install 失败，所以列表要保守
const SHELL_ASSETS = [
  '/manifest.webmanifest',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
  '/icons/apple-touch-icon.png',
  '/icons/favicon-32.png'
];

// 永不缓存的路径前缀
const BYPASS = ['/api/', '/stream_live', '/stream_pcm', '/url/', '/ext_audio', '/sw.js'];

self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(SHELL_CACHE);
    // 逐个 add，单个失败不影响整体
    await Promise.all(SHELL_ASSETS.map(async (url) => {
      try { await cache.add(new Request(url, { cache: 'reload' })); } catch (e) {}
    }));
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const keys = await caches.keys();
    await Promise.all(keys.filter(k => k !== SHELL_CACHE).map(k => caches.delete(k)));
    await self.clients.claim();
  })());
});

function shouldBypass(url) {
  if (url.origin !== self.location.origin) return true;
  return BYPASS.some(p => url.pathname.startsWith(p));
}

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (shouldBypass(url)) return;

  // 页面导航：网络优先，失败用缓存里的首页（离线壳）
  if (req.mode === 'navigate') {
    event.respondWith((async () => {
      try {
        const fresh = await fetch(req);
        const cache = await caches.open(SHELL_CACHE);
        // 只缓存根页面，其它页面（/lyrics 等）本来就是服务端渲染的小文件
        if (url.pathname === '/') cache.put('/', fresh.clone());
        return fresh;
      } catch (e) {
        const cache = await caches.open(SHELL_CACHE);
        const hit = await cache.match('/');
        return hit || new Response('离线了，且没有缓存页面', {
          status: 503, headers: { 'Content-Type': 'text/plain; charset=utf-8' }
        });
      }
    })());
    return;
  }

  // 静态资源：缓存优先，后台顺手更新
  event.respondWith((async () => {
    const cache = await caches.open(SHELL_CACHE);
    const hit = await cache.match(req);
    if (hit) {
      fetch(req).then((resp) => {
        if (resp && resp.ok) cache.put(req, resp.clone());
      }).catch(() => {});
      return hit;
    }
    const resp = await fetch(req);
    if (resp && resp.ok && (url.pathname.startsWith('/icons/') ||
        url.pathname === '/manifest.webmanifest')) {
      cache.put(req, resp.clone());
    }
    return resp;
  })());
});
