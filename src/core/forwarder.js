/**
 * Auto Forwarder - Uploads downloaded media to a destination channel
 * Supports: Single Aggregation Channel, Custom Destination, Delete after forward
 */

import fs from 'fs/promises';
import path from 'path';
import { Api } from 'telegram';
import { colorize } from '../cli/colors.js';
import { TelegramBotManager } from './telegram-bot.js';

export class AutoForwarder {
    constructor(client, config, accountManager = null) {
        this.client = client;
        this.config = config;
        this.accountManager = accountManager;
        this.botManager = new TelegramBotManager(config);
        this.storageChannelId = null; // Cache for the single storage channel
        this._sendChain = Promise.resolve();
        this._lastSendAt = 0;
    }

    /**
     * Main processing entry point
     * @param {Object} downloadInfo - From downloader 'download_complete' event
     */
    async process(downloadInfo) {
        const { filePath, groupId, groupName, message, deduped } = downloadInfo;

        // A SHA-256 duplicate can come from a different source group. Keep
        // the shared local file, but do not post it to the destination again.
        if (deduped) {
            console.log(
                colorize(`⏭️  [AutoForward] Skipping duplicate for ${groupName}...`, 'gray'),
            );
            return;
        }

        // 1. Check Group Config
        const groupConfig = this.config.groups.find((g) => String(g.id) === String(groupId));
        if (!groupConfig || !groupConfig.autoForward || !groupConfig.autoForward.enabled) {
            return;
        }

        const settings = groupConfig.autoForward;
        this.botManager.config = this.config;

        const bot = settings.botId ? this.botManager.get(settings.botId) : null;
        if (settings.botId && !bot)
            throw new Error(`Configured Telegram bot not found: ${settings.botId}`);
        if (settings.protectContent === true && !bot) {
            throw new Error('Protected forwarding requires a BotFather bot, not a user account');
        }
        if (
            bot &&
            (!settings.destination ||
                settings.destination === 'storage' ||
                settings.destination === 'me')
        ) {
            throw new Error(
                'Bot forwarding requires an explicit Telegram destination ID or @username',
            );
        }

        // Use per-group forward account if configured
        const fwdClient =
            this.accountManager && groupConfig.forwardAccount
                ? this.accountManager.getClient(groupConfig.forwardAccount)
                : this.client;

        console.log(colorize(`➡️  [AutoForward] Processing for ${groupName}...`, 'cyan'));

        try {
            // 2. Resolve Destination
            let targetPeer = bot
                ? settings.destination
                : await this.resolveDestination(settings.destination, fwdClient);
            if (!targetPeer) {
                console.log(
                    colorize(`⚠️  [AutoForward] Could not resolve destination. Skipping.`, 'yellow'),
                );
                return;
            }

            // 3. Build the destination caption from per-group forwarding settings.
            const captionMode = ['copy', 'none', 'source'].includes(settings.captionMode)
                ? settings.captionMode
                : 'none';
            let caption = captionMode === 'none' ? '' : message?.message || message?.text || '';
            if (captionMode === 'source') {
                const msgId = message?.id;
                const cleanId = String(groupId).replace(/^-100/, '');
                caption =
                    msgId && String(groupId).startsWith('-100')
                        ? `Source: [${groupName}](https://t.me/c/${cleanId}/${msgId})`
                        : `Source: ${groupName}`;
            }
            const replacements = Array.isArray(settings.captionReplacements)
                ? settings.captionReplacements
                : [];
            for (const rule of replacements) {
                if (typeof rule?.find !== 'string' || typeof rule?.replace !== 'string') continue;
                try {
                    caption = rule.regex
                        ? caption.replace(new RegExp(rule.find, 'g'), rule.replace)
                        : caption.split(rule.find).join(rule.replace);
                } catch {
                    // Ignore one invalid rule and keep forwarding the media.
                }
            }
            if (captionMode !== 'none') {
                caption = `${settings.captionPrefix || ''}${caption}${settings.captionSuffix || ''}`;
            } else {
                caption = '';
            }

            // 4. Upload & Send
            // We use sendFile to bypass restricted content forwarding
            const sentMsg = await this._sendWithThrottle(() =>
                bot
                    ? bot.sendFile(targetPeer, filePath, {
                          caption,
                          messageThreadId: Number.isFinite(Number(settings.destinationTopicId))
                              ? Number(settings.destinationTopicId)
                              : undefined,
                          protectContent: settings.protectContent === true,
                          hasSpoiler: settings.nsfwSpoiler === true,
                      })
                    : fwdClient.sendFile(targetPeer, {
                          file: filePath,
                          caption,
                          forceDocument: false,
                          workers: 1,
                      }),
            );

            // GramJS returns the new message; surface its TG message-id in the log
            // so operators can trace the destination copy back from the dashboard.
            const sentMsgId = sentMsg?.id ?? sentMsg?.message?.id ?? null;
            const dest = settings.destination || 'Storage Channel';
            const tail = sentMsgId ? ` (msg #${sentMsgId})` : '';
            console.log(colorize(`✅ [AutoForward] Sent to ${dest}${tail}`, 'green'));

            // 5. Cleanup (if enabled). Isolate the unlink in its own
            // try/catch so a successful upload isn't reported as failed
            // when the local delete races with another process. The
            // hourly integrity sweep will eventually drop the orphan
            // DB row whose file is gone (or here, whose file we
            // intentionally couldn't delete).
            if (settings.deleteAfterForward) {
                try {
                    await fs.unlink(filePath);
                    console.log(
                        colorize(
                            `🗑️  [AutoForward] Deleted local file: ${path.basename(filePath)}`,
                            'gray',
                        ),
                    );
                } catch (unlinkErr) {
                    console.warn(
                        colorize(
                            `⚠️  [AutoForward] Forwarded but local delete failed for ${path.basename(filePath)}: ${unlinkErr.message}`,
                            'yellow',
                        ),
                    );
                }
            }
        } catch (error) {
            console.log(colorize(`❌ [AutoForward] Error: ${error.message}`, 'red'));
        }
    }

    async _sendWithThrottle(send) {
        const run = this._sendChain.then(async () => {
            const intervalMs = Math.max(
                1000,
                Number(this.config.advanced?.forwarding?.minIntervalMs) || 1500,
            );
            const waitMs = Math.max(0, intervalMs - (Date.now() - this._lastSendAt));
            if (waitMs) await new Promise((resolve) => setTimeout(resolve, waitMs));
            this._lastSendAt = Date.now();
            return send();
        });
        // ponytail: one global queue keeps burst traffic conservative; split by destination only if throughput becomes necessary.
        this._sendChain = run.catch(() => {});
        return run;
    }

    /**
     * Resolve where to send the file
     */
    async resolveDestination(destination, client) {
        client = client || this.client;
        // Case A: Specific Destination
        if (destination && destination !== 'storage') {
            // Saved Messages — accept both 'me' and 'saved' aliases.
            if (destination === 'me' || destination === 'saved') return 'me';

            // Try to parse if it's an ID
            if (/^-?\d+$/.test(destination)) {
                try {
                    const id = BigInt(destination);

                    // Prefer the full entity so GramJS carries the current peer metadata.
                    try {
                        const entity = await client.getEntity(id);
                        if (entity) return entity;
                    } catch {
                        /* fall through */
                    }

                    // Secondary: use the cached InputPeer when the full entity is unavailable.
                    try {
                        return await client.getInputEntity(id);
                    } catch {
                        /* fall through */
                    }

                    // Last resort: hand-roll an InputPeer from the canonical -100… layout.
                    // accessHash=0 only resolves for channels the bot/user has interacted with
                    // server-side; for fully-private channels the send will fail and the caller
                    // will see a clear error (CHANNEL_INVALID / PEER_ID_INVALID) instead of a
                    // mysterious resolve hang. Logging the fallback so operators can spot it.
                    const raw = String(destination);
                    if (raw.startsWith('-100')) {
                        console.warn(
                            colorize(
                                `⚠️  [AutoForward] Falling back to manual InputPeerChannel for ${raw} — peer is not in dialog cache. If sends fail, open the channel once from the configured account.`,
                                'yellow',
                            ),
                        );
                        return new Api.InputPeerChannel({
                            channelId: BigInt(raw.replace(/^-100/, '')),
                            accessHash: BigInt(0),
                        });
                    }
                    if (raw.startsWith('-')) {
                        console.warn(
                            colorize(
                                `⚠️  [AutoForward] Falling back to manual InputPeerChat for ${raw}.`,
                                'yellow',
                            ),
                        );
                        return new Api.InputPeerChat({ chatId: BigInt(raw.replace(/^-/, '')) });
                    }
                    return id;
                } catch {
                    return destination;
                }
            }

            // Treat as username or phone
            return destination;
        }

        // Case B: Auto Storage Channel (Single Channel)
        if (this.storageChannelId) return this.storageChannelId;

        // Try to find existing "Telegram Downloader Storage" in dialogs
        try {
            const dialogs = await client.getDialogs({ limit: 100 });
            const found = dialogs.find((d) => d.title === 'Telegram Downloader Storage');

            if (found) {
                this.storageChannelId = found.entity;
                return this.storageChannelId;
            }

            // Create new if not found
            console.log(colorize(`🛠️  [AutoForward] Creating storage channel...`, 'cyan'));
            const result = await client.invoke(
                new Api.channels.CreateChannel({
                    title: 'Telegram Downloader Storage',
                    about: 'Auto-forwarded media storage from Telegram Media Downloader',
                    broadcast: true,
                    megagroup: false,
                }),
            );

            // Access the created channel
            if (result.chats && result.chats[0]) {
                this.storageChannelId = result.chats[0];
                console.log(
                    colorize(
                        `✅ [AutoForward] Created channel: Telegram Downloader Storage`,
                        'green',
                    ),
                );
                return this.storageChannelId;
            }
        } catch (e) {
            console.log(
                colorize(
                    `❌ [AutoForward] Failed to create/find storage channel: ${e.message}`,
                    'red',
                ),
            );
        }

        return null;
    }
}
