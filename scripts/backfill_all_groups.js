import { loadConfig } from '../src/config/manager.js';
import { AccountManager } from '../src/core/accounts.js';
import { DownloadManager } from '../src/core/downloader.js';
import { HistoryDownloader } from '../src/core/history.js';
import { RateLimiter } from '../src/core/security.js';
import { AutoForwarder } from '../src/core/forwarder.js';

const config = loadConfig();
const accountManager = new AccountManager(config);
const loaded = await accountManager.loadAll();
if (!loaded) throw new Error('No Telegram account session loaded');

const client = accountManager.getDefaultClient();
const downloader = new DownloadManager(client, config, new RateLimiter(config.rateLimits));
const forwarder = new AutoForwarder(client, config, accountManager);
await downloader.init();
downloader.on('download_complete', (info) =>
    forwarder.process(info).catch((error) => console.error('[forward]', error.message)),
);
downloader.start();

const waitForQueue = async () => {
    while (downloader.pendingCount > 0 || downloader.active.size > 0) {
        await new Promise((resolve) => setTimeout(resolve, 1000));
    }
};

try {
    for (const group of config.groups || []) {
        const history = new HistoryDownloader(client, downloader, config, accountManager);
        history.on('progress', (stats) => {
            if (stats.processed % 100 === 0) {
                console.log(
                    `[backfill] ${group.name}: processed=${stats.processed} downloaded=${stats.downloaded}`,
                );
            }
        });
        console.log(`[backfill] START ${group.name} (${group.id}) — all history`);
        try {
            await history.downloadHistory(group.id, { limit: undefined });
            await waitForQueue();
            console.log(
                `[backfill] DONE ${group.name}: processed=${history.stats.processed} downloaded=${history.stats.downloaded}`,
            );
        } catch (error) {
            console.error(`[backfill] ERROR ${group.name}: ${error.message}`);
        }
    }
} finally {
    await waitForQueue();
    await downloader.stop();
    await accountManager.disconnectAll();
}
