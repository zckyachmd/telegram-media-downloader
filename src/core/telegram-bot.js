import { openAsBlob } from 'node:fs';
import crypto from 'node:crypto';
import { getOrGenerateSecret } from './secret.js';
import { SecureSession } from './security.js';

const TOKEN_PASSWORD = getOrGenerateSecret();
const TOKEN_SECURE = new SecureSession(TOKEN_PASSWORD);

export function encryptBotToken(token) {
    return TOKEN_SECURE.encrypt(String(token).trim());
}

function decryptBotToken(value) {
    if (!value) return '';
    if (typeof value === 'string') return value;
    return TOKEN_SECURE.decrypt(value);
}

export class TelegramBotClient {
    constructor({ id, name, token }) {
        this.id = id;
        this.name = name || id;
        this.token = token;
    }

    async call(method, fields = {}, fileField = null, filePath = null) {
        const form = new FormData();
        for (const [key, value] of Object.entries(fields)) {
            if (value !== undefined && value !== null && value !== '')
                form.append(key, String(value));
        }
        if (fileField && filePath) {
            const blob = await openAsBlob(filePath);
            form.append(fileField, blob, filePath.split(/[\\/]/).pop());
        }

        const response = await fetch(`https://api.telegram.org/bot${this.token}/${method}`, {
            method: 'POST',
            body: form,
        });
        const body = await response.json().catch(() => ({}));
        if (!response.ok || !body.ok) {
            throw new Error(
                body.description || `Telegram Bot API ${method} failed (${response.status})`,
            );
        }
        return body.result;
    }

    async getMe() {
        const response = await fetch(`https://api.telegram.org/bot${this.token}/getMe`);
        const body = await response.json().catch(() => ({}));
        if (!response.ok || !body.ok) {
            throw new Error(
                body.description || `Telegram Bot API getMe failed (${response.status})`,
            );
        }
        return body.result;
    }

    async sendFile(chatId, filePath, options = {}) {
        const ext = filePath.toLowerCase().split('.').pop();
        const isPhoto = ['jpg', 'jpeg', 'png', 'webp'].includes(ext);
        const isVideo = ['mp4', 'mov', 'm4v', 'webm'].includes(ext);
        const method = isPhoto ? 'sendPhoto' : isVideo ? 'sendVideo' : 'sendDocument';
        const fileField = isPhoto ? 'photo' : isVideo ? 'video' : 'document';
        const fields = {
            chat_id: chatId,
            caption: options.caption || undefined,
            message_thread_id: options.messageThreadId,
            protect_content: options.protectContent === true ? true : undefined,
            has_spoiler: options.hasSpoiler === true && (isPhoto || isVideo) ? true : undefined,
        };
        if (isVideo) fields.supports_streaming = true;
        return this.call(method, fields, fileField, filePath);
    }
}

export class TelegramBotManager {
    constructor(config) {
        this.config = config;
    }

    list() {
        return (this.config.telegram?.bots || []).map((bot) => ({
            id: bot.id,
            name: bot.name || bot.id,
            username: bot.username || '',
            configured: !!bot.tokenEncrypted,
            verified: bot.verified === true,
            verifiedAt: bot.verifiedAt || null,
        }));
    }

    get(id) {
        const bot = (this.config.telegram?.bots || []).find(
            (item) => String(item.id) === String(id),
        );
        if (!bot?.tokenEncrypted) return null;
        return new TelegramBotClient({
            id: bot.id,
            name: bot.name,
            token: decryptBotToken(bot.tokenEncrypted),
        });
    }

    static makeId(username) {
        return String(username || `bot-${crypto.randomBytes(4).toString('hex')}`)
            .toLowerCase()
            .replace(/[^a-z0-9_-]/g, '-');
    }
}
